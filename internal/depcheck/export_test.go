package depcheck

import (
	"context"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// blobExportSource is a minimal ExportSource backed by pre-computed blobs,
// standing in for depexport.Cache's real, possibly-checking behavior: every
// pkgPath resolve tests against here is already fully decoded ahead of
// time, so ExportDataComplete never itself triggers a from-source check.
type blobExportSource struct {
	blobs map[string][]byte
}

func (s blobExportSource) ExportDataComplete(pkgPath string) (data []byte, complete, ok bool, err error) {
	data, ok = s.blobs[pkgPath]
	return data, true, ok, nil
}

// TestExportResolver_Resolve_WorkingSetAboveShrunkCapStaysResident is the
// regression test for the connect-go generic-field hang: exportResolver.
// resolve evicts r's entire decode cache (fset, cache, pkgs, complete)
// whenever r.cache.Bytes() exceeds exportDecodeCap, BEFORE decoding the
// path currently being resolved (see resolve's own doc). A caller resolving
// a large transitive closure one pkgPath at a time — exactly how
// ctxImporter drives this — needs every package already decoded for that
// closure to stay resident and identity-stable until the whole closure
// resolves; if the cap were compared against a scaled-up estimate of the
// closure's decoded heap instead of its own literal blob-byte sum (a
// revision this test guards against reintroducing), a closure whose real
// blob bytes sit well under exportDecodeCap could still cross the shrunk
// threshold mid-closure, evicting already-decoded packages and forcing
// their re-decode — or, for a closure whose working set never shrinks
// below the shrunk threshold, never converging at all.
//
// The four fixture packages here sum to comfortably under exportDecodeCap
// (256MiB) — so the real cap this test exercises never evicts anything —
// but the first three alone sum to more than exportDecodeCap/10, the
// effective threshold a Bytes()*10 comparison would enforce. Under that
// comparison, resolving the fourth package would first evict all three
// already-resolved ones.
func TestExportResolver_Resolve_WorkingSetAboveShrunkCapStaysResident(t *testing.T) {
	const (
		numFat       = 4
		nDeclsPerFat = 450000 // ~10MiB of gcexportdata per package
	)

	dir := t.TempDir()
	writeStarFixture(t, dir, 1, numFat, nDeclsPerFat, 0)
	meta := starMetadataSource{dir: dir, numLeaves: 1, numFat: numFat}
	p := NewProvider(meta, Options{})

	blobs := make(map[string][]byte, numFat)
	var total int64
	for k := range numFat {
		cp, err := p.Package(context.Background(), starFatPkgPath(k))
		if err != nil {
			t.Fatalf("Package(%s): %v", starFatPkgPath(k), err)
		}
		blob, err := typecheck.WriteExport(cp.Types(), p.FileSet())
		if err != nil {
			t.Fatalf("WriteExport(%s): %v", starFatPkgPath(k), err)
		}
		blobs[starFatPkgPath(k)] = blob
		total += int64(len(blob))
	}
	t.Logf("total blob bytes across %d packages = %d (%.2f MiB); exportDecodeCap/10 = %.2f MiB, exportDecodeCap = %.2f MiB",
		numFat, total, float64(total)/(1024*1024), float64(exportDecodeCap)/10/(1024*1024), float64(exportDecodeCap)/(1024*1024))
	if total <= exportDecodeCap/10 {
		t.Fatalf("fixture total %d bytes must exceed exportDecodeCap/10 (%d) to exercise the regression this test guards", total, exportDecodeCap/10)
	}
	if total >= exportDecodeCap {
		t.Fatalf("fixture total %d bytes must stay under exportDecodeCap (%d)", total, exportDecodeCap)
	}

	r := newExportResolver(blobExportSource{blobs: blobs})
	ctx := context.Background()
	for k := range numFat {
		path := starFatPkgPath(k)
		pkg, complete, ok, err := r.resolve(ctx, path)
		if err != nil {
			t.Fatalf("resolve(%s): %v", path, err)
		}
		if !ok || pkg == nil {
			t.Fatalf("resolve(%s) = ok=%v pkg=%v, want ok=true and a non-nil package", path, ok, pkg)
		}
		if !complete {
			t.Errorf("resolve(%s) complete = false, want true", path)
		}
	}

	for k := range numFat {
		path := starFatPkgPath(k)
		pkg, _, ok := r.get(path)
		if !ok || pkg == nil {
			t.Errorf("r.get(%s) after resolving all %d packages: ok=%v pkg=%v, want both true and non-nil (a cap scaled down against the closure's decoded-heap estimate would have evicted earlier packages before the closure finished resolving)", path, numFat, ok, pkg)
		}
	}
	if got := r.cache.Decodes(); got != numFat {
		t.Errorf("cache.Decodes() = %d, want exactly %d (one decode per distinct package; a mid-closure eviction would force at least one re-decode)", got, numFat)
	}
}
