package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/store"
)

// corruptExportDataForTest overwrites pkgPath's stored CAS blob so its
// Export field decodes to nothing usable, while leaving its Facts field
// untouched -- mirroring internal/xref's own corruptExportData (see
// implementation_decodefailure_test.go), reimplemented here since this
// package's fallback path is deliberately independent of internal/xref
// (see implementationLiveFallback's doc in handlers_xref.go).
func corruptExportDataForTest(t *testing.T, db *store.DB, cas *store.CAS, pkgPath string) {
	t.Helper()
	pkgHash := store.Hash(pkgPath)
	ptr, err := db.GetUnit(context.Background(), pkgHash)
	if err != nil {
		t.Fatalf("GetUnit(%s): %v", pkgPath, err)
	}
	blob, ok, err := cas.Get(context.Background(), ptr.BlobKey)
	if err != nil || !ok {
		t.Fatalf("CAS.Get(%s) ok=%v err=%v", pkgPath, ok, err)
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		t.Fatalf("DecodeUnitBlob(%s): %v", pkgPath, err)
	}
	u.Export = []byte("not valid gc export data")
	if err := cas.Put(ptr.BlobKey, store.EncodeUnitBlob(&u)); err != nil {
		t.Fatalf("CAS.Put(%s): %v", pkgPath, err)
	}
}

// implementationResultLocations extracts result's protocol.Location slice,
// failing the test if result is not a protocol.LocationSlice.
func implementationResultLocations(t *testing.T, result any) protocol.LocationSlice {
	t.Helper()
	locs, ok := result.(protocol.LocationSlice)
	if !ok {
		t.Fatalf("handleImplementation: result = %#v (%T), want protocol.LocationSlice", result, result)
	}
	return locs
}

// TestHandleImplementation_MethodOnInterfaceListsImplementer is the
// baseline (no decode failure) case: querying Implementation on interface
// I's own F method (testdata/module/typeh/typeh.go) must list S's F.
func TestHandleImplementation_MethodOnInterfaceListsImplementer(t *testing.T) {
	s, snap, _ := newTestServer(t)
	file := snap.Packages["example.com/servermod/typeh"].GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	pos := identPositionIn(t, file, data, "F", 1) // I's own F declaration

	result, err := s.handleImplementation(context.Background(), mustMarshal(t, &protocol.ImplementationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleImplementation: %v", err)
	}
	locs := implementationResultLocations(t, result)
	if len(locs) == 0 {
		t.Fatal("handleImplementation(I.F) = no locations, want at least S.F")
	}
}

// TestHandleImplementation_LiveFallback_QueriedPackageDecodeFailure is the
// regression test for the root cause behind "no implementation found" on a
// warm, fully built index: internal/xref's Resolver.Implementation always
// needs to decode the QUERIED interface/method's OWN declaring package's
// export data (never a candidate's -- see internal/xref's doc.go), so once
// that specific decode fails -- reproduced here via corruptExportDataForTest
// on "example.com/servermod/typeh" itself, simulating an export-data cache
// eviction or a not-yet-persisted dependency -- it used to come back as a
// silent empty result. handleImplementation must now answer via
// implementationLiveFallback instead, sourced from the file's own live,
// already type-checked go/types info rather than the now-undecodable
// export data, both for a query on the interface's method (I.F) and on the
// interface's own name (I).
func TestHandleImplementation_LiveFallback_QueriedPackageDecodeFailure(t *testing.T) {
	s, snap, _ := newTestServer(t)
	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("s.idx.Load() = nil, want an installed index")
	}
	corruptExportDataForTest(t, idx.db, idx.cas, "example.com/servermod/typeh")

	file := snap.Packages["example.com/servermod/typeh"].GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	t.Run("method", func(t *testing.T) {
		pos := identPositionIn(t, file, data, "F", 1) // I's own F declaration
		result, err := s.handleImplementation(context.Background(), mustMarshal(t, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if err != nil {
			t.Fatalf("handleImplementation(I.F): %v", err)
		}
		locs := implementationResultLocations(t, result)
		if len(locs) == 0 {
			t.Fatal("handleImplementation(I.F) with corrupted export data = no locations, want the live fallback to still find S.F")
		}
	})

	t.Run("type", func(t *testing.T) {
		pos := identPositionIn(t, file, data, "I", 1) // interface I's own declaration
		result, err := s.handleImplementation(context.Background(), mustMarshal(t, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if err != nil {
			t.Fatalf("handleImplementation(I): %v", err)
		}
		locs := implementationResultLocations(t, result)
		if len(locs) == 0 {
			t.Fatal("handleImplementation(I) with corrupted export data = no locations, want the live fallback to still find S")
		}
	})
}
