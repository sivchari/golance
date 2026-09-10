package depexport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
)

// brokenMetadataSource reports a single package, "broken", whose only
// import is a path this source does not know at all — standing in for a
// transiently-unresolvable transitive dependency (H9's scenario), without
// needing a real, flaky module-graph condition to reproduce it.
type brokenMetadataSource struct {
	dir string
}

func (b brokenMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	if pkgPath != "broken" {
		return "", nil, nil, false
	}
	return b.dir, []string{filepath.Join(b.dir, "broken.go")}, []string{"missing"}, true
}

func writeBrokenFixture(t *testing.T, dir string) {
	t.Helper()
	src := "package broken\n\nimport \"missing\"\n\n// V references the unresolvable import.\nvar V = missing.V\n"
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write broken.go: %v", err)
	}
}

// TestCache_DoesNotPersistIncompleteCheck verifies H9's fix: ExportData
// still resolves a package whose own import could not be resolved — usable
// for THIS call's own caller, mirroring depcheck's own best-effort
// degrade-not-fail contract — but never persists that result to the
// machine-global CAS, even when its directory would otherwise be treated as
// immutable (GOROOT/GOModCache). Without this, the degraded blob would be
// served to every repository on the machine for up to GCMaxAge.
func TestCache_DoesNotPersistIncompleteCheck(t *testing.T) {
	dir := t.TempDir()
	writeBrokenFixture(t, dir)
	meta := brokenMetadataSource{dir: dir}
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cas := newTestCAS(t)
	// Point GOROOT at dir itself so immutable(dir) is true despite this
	// being a throwaway temp directory — isolating this test to the
	// Incomplete gate, not the pre-existing directory-identity gate
	// TestCache_DoesNotPersistOutsideImmutableDirs already covers.
	cache := NewCache(cas, meta, provider, Options{GOROOT: dir})

	data, ok, err := cache.ExportData("broken")
	if err != nil {
		t.Fatalf("ExportData(broken): %v", err)
	}
	if !ok {
		t.Fatal("ExportData(broken): ok = false, want true (still resolves, just degraded)")
	}
	if len(data) == 0 {
		t.Error("ExportData(broken): empty data")
	}

	key := cache.digest("broken", dir)
	if cas.Has(key) {
		t.Error("CAS holds a blob for a package whose own import could not be resolved; it must never be persisted")
	}
}
