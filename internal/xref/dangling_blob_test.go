package xref

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
)

// danglingBlobPath mirrors (*store.CAS)'s own on-disk blob sharding (see
// its doc), letting a test remove one specific blob file directly to
// simulate a [store.CAS.GC] pass racing an incomplete mark set — the exact
// scenario that leaves a recorded [store.UnitPointer].BlobKey dangling.
func danglingBlobPath(casDir string, key uint64) string {
	hex := fmt.Sprintf("%016x", key)
	return filepath.Join(casDir, hex[:2], hex[2:]+".blob")
}

// deleteBlob removes pkgPath's currently recorded blob from the CAS at
// casDir, without touching db's own UnitPointer — reproducing a dangling
// pointer.
func deleteBlob(t *testing.T, db *store.DB, casDir, pkgPath string) {
	t.Helper()
	ptr, err := db.GetUnit(context.Background(), store.Hash(pkgPath))
	if err != nil {
		t.Fatalf("GetUnit(%s): %v", pkgPath, err)
	}
	if err := os.Remove(danglingBlobPath(casDir, ptr.BlobKey)); err != nil {
		t.Fatalf("remove blob for %s: %v", pkgPath, err)
	}
}

// TestUnitBlob_DistinguishesNotIndexedFromDanglingBlob verifies unitBlob's
// two [store.ErrNotFound]-wrapping error messages: one for a package never
// indexed at all (no UnitPointer recorded), the other for a recorded
// UnitPointer whose blob has gone missing from the CAS — a dangling pointer
// (see internal/index's processUnit and packageChanged for the write-side
// half of this fix). Both must still satisfy errors.Is(err,
// store.ErrNotFound), since existing callers rely on that.
func TestUnitBlob_DistinguishesNotIndexedFromDanglingBlob(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	casDir := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casDir)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if _, err := index.Build(context.Background(), snap, db, cas, &index.Options{}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	r := New(db, cas, snap, false)

	notIndexed := store.Hash("example.com/xrefmod/doesnotexist")
	_, err = r.unitBlob(context.Background(), notIndexed)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unitBlob(never indexed) error = %v, want it to wrap store.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "no facts recorded for") {
		t.Errorf("unitBlob(never indexed) error = %q, want it to mention \"no facts recorded for\"", err.Error())
	}

	deleteBlob(t, db, casDir, pkgIface)
	_, err = r.unitBlob(context.Background(), store.Hash(pkgIface))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unitBlob(dangling pointer) error = %v, want it to wrap store.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "missing from CAS") {
		t.Errorf("unitBlob(dangling pointer) error = %q, want it to mention \"missing from CAS\"", err.Error())
	}
	if strings.Contains(err.Error(), "no facts recorded for") {
		t.Errorf("unitBlob(dangling pointer) error = %q, must not read like the never-indexed case", err.Error())
	}
}
