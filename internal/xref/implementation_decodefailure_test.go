package xref

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// corruptExportData overwrites pkgPath's stored CAS blob so its Export
// field decodes to nothing usable, while leaving its Facts field (and
// therefore its name/method index entries -- what makes it a LookupMethod
// candidate in the first place) untouched. This simulates "the dependency's
// export data becoming unavailable at query time" (the seam the real
// monorepo report's hypothesis names -- see implementation.go's implDiag
// doc) without needing an actual GOCACHE eviction or a second module: any
// package's own stored blob can go missing or get corrupted the same way,
// whether or not its signatures reference a module dependency.
func corruptExportData(t *testing.T, db *store.DB, cas *store.CAS, pkgPath string) {
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

// TestImplementation_SkipsUndecodableCandidateInsteadOfAborting is the
// robustness fix implDiag/logImplDiag's doc describes: implementingTypes
// used to silently `continue` past a candidate whose export data failed to
// decode, which -- on its own, with a healthy candidate elsewhere -- was
// already harmless (the candidate loop simply moved on). This pins that
// the harmless case stays harmless: one candidate's export data going bad
// must never take a healthy candidate down with it.
func TestImplementation_SkipsUndecodableCandidateInsteadOfAborting(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/decodefail\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Repo interface {
	Save() error
}
`)
	// Broken's receiver is generic so its methods are never fingerprinted
	// (see registerMethodSet's doc): this keeps the test exercising the
	// decode-based confirmation fallback implementingTypes still uses for a
	// candidate fingerprint confirmation cannot trust, which is exactly what
	// corruptExportData needs to matter here. A non-generic Broken would now
	// be confirmed by Fingerprint alone, needing no decode at all — the
	// whole point of the unexported-implementer fix this test predates.
	writeTestFile(t, dir, "broken/broken.go", `package broken

type Broken[T any] struct{}

func (b Broken[T]) Save() error { return nil }
`)
	writeTestFile(t, dir, "healthy/healthy.go", `package healthy

type Healthy struct{}

func (h Healthy) Save() error { return nil }
`)

	r, snap, db, cas := newResolverAndStoreForDir(t, dir)
	corruptExportData(t, db, cas, "example.com/decodefail/broken")

	var logBuf bytes.Buffer
	r.SetLogger(log.New(&logBuf, "", 0))

	ifaceFile := goFile(t, snap, "example.com/decodefail/iface", "iface.go")
	healthyFile := goFile(t, snap, "example.com/decodefail/healthy", "healthy.go")

	line, col := identOccurrence(t, ifaceFile, "Repo")
	locs, err := r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Repo): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Repo) = %+v, want exactly 1 result (Healthy): Broken's undecodable export data must be skipped, not abort the whole query", locs)
	}
	wantLoc(t, locs, healthyFile, "Healthy")
}

// TestImplementation_EmptyResultLogsDiagnostics pins requirement (a) of the
// same fix: when EVERY candidate's export data fails to decode, the final
// result is legitimately empty -- but unlike an interface with genuinely no
// implementers, this leaves a server-side trail (via Resolver.SetLogger)
// naming the undecodable candidate and its package path, so it is no longer
// indistinguishable from "genuinely no implementers" purely from the
// server's own log.
func TestImplementation_EmptyResultLogsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/decodefailempty\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Repo interface {
	Save() error
}
`)
	// Broken's receiver is generic so its methods are never fingerprinted
	// (see registerMethodSet's doc): this keeps the test exercising the
	// decode-based confirmation fallback implementingTypes still uses for a
	// candidate fingerprint confirmation cannot trust, which is exactly what
	// corruptExportData needs to matter here. A non-generic Broken would now
	// be confirmed by Fingerprint alone, needing no decode at all — the
	// whole point of the unexported-implementer fix this test predates.
	writeTestFile(t, dir, "broken/broken.go", `package broken

type Broken[T any] struct{}

func (b Broken[T]) Save() error { return nil }
`)

	r, snap, db, cas := newResolverAndStoreForDir(t, dir)
	corruptExportData(t, db, cas, "example.com/decodefailempty/broken")

	var logBuf bytes.Buffer
	r.SetLogger(log.New(&logBuf, "", 0))

	ifaceFile := goFile(t, snap, "example.com/decodefailempty/iface", "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Repo")
	locs, err := r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Repo): %v", err)
	}
	if len(locs) != 0 {
		t.Fatalf("Implementation(Repo) = %+v, want no results (Broken's only candidate is undecodable)", locs)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "example.com/decodefailempty/broken") {
		t.Errorf("log output = %q, want it to name the undecodable candidate's package path", logged)
	}
	if !strings.Contains(logged, "undecodable") {
		t.Errorf("log output = %q, want it to explain the candidate was skipped for a decode failure", logged)
	}
	if !strings.Contains(logged, "Save") {
		t.Errorf("log output = %q, want it to mention the queried method name", logged)
	}
}

// TestImplementation_MethodNameQuery_SkipsUndecodableCandidate is
// TestImplementation_SkipsUndecodableCandidateInsteadOfAborting's
// method-name-granular counterpart: the real report this fix responds to
// was specifically "Go to Implementation on an interface METHOD NAME"
// (methodImplementationSymbols, not implementationsOfInterface), so this
// pins the same robustness on that path too.
func TestImplementation_MethodNameQuery_SkipsUndecodableCandidate(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/decodefailmethod\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Repo interface {
	Save() error
}
`)
	// Broken's receiver is generic so its methods are never fingerprinted
	// (see registerMethodSet's doc): this keeps the test exercising the
	// decode-based confirmation fallback implementingTypes still uses for a
	// candidate fingerprint confirmation cannot trust, which is exactly what
	// corruptExportData needs to matter here. A non-generic Broken would now
	// be confirmed by Fingerprint alone, needing no decode at all — the
	// whole point of the unexported-implementer fix this test predates.
	writeTestFile(t, dir, "broken/broken.go", `package broken

type Broken[T any] struct{}

func (b Broken[T]) Save() error { return nil }
`)
	writeTestFile(t, dir, "healthy/healthy.go", `package healthy

type Healthy struct{}

func (h Healthy) Save() error { return nil }
`)

	r, snap, db, cas := newResolverAndStoreForDir(t, dir)
	corruptExportData(t, db, cas, "example.com/decodefailmethod/broken")

	ifaceFile := goFile(t, snap, "example.com/decodefailmethod/iface", "iface.go")
	healthyFile := goFile(t, snap, "example.com/decodefailmethod/healthy", "healthy.go")

	line, col := identOccurrence(t, ifaceFile, "Save")
	locs, err := r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Repo.Save): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Repo.Save) = %+v, want exactly 1 result (Healthy.Save)", locs)
	}
	wantLoc(t, locs, healthyFile, "Save")
}
