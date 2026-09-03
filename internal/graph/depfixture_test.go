package graph

import (
	"path/filepath"
	"testing"
)

// depfixturePkg and depfixtureDep are the two packages
// testdata/depfixture's own module graph contains: the Root package itself,
// and its one real external (module-cache) dependency. Used by
// repokey_test.go to verify a real, non-root module-cache dependency's
// Dir/GoFiles round-trip correctly through SaveCache/LoadCache.
const (
	depfixturePkg = "example.com/depfixture"
	depfixtureDep = "golang.org/x/sync/singleflight"
)

// loadDepFixture loads testdata/depfixture — a minimal module with one real
// external module-cache dependency (see its own go.mod).
func loadDepFixture(t *testing.T) *Snapshot {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "depfixture"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := Load(Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return snap
}

// TestLoad_RootPackagesAreMarkedRoot verifies Load correctly distinguishes
// depfixture's own Root package from its non-root dependency — the
// distinction internal/depexport relies on to decide which packages it may
// ever persist a checked export-data blob for (see its own "Cache identity"
// doc): a Root package is always type-checked straight from source (see
// internal/index/scheduler.go's doc and internal/check) and never has its
// own export data produced or consulted at all.
func TestLoad_RootPackagesAreMarkedRoot(t *testing.T) {
	snap := loadDepFixture(t)

	pkg, ok := snap.Package(depfixturePkg)
	if !ok {
		t.Fatalf("Package(%s) missing", depfixturePkg)
	}
	if !pkg.Root {
		t.Fatalf("Package(%s).Root = false, want true", depfixturePkg)
	}

	dep, ok := snap.Package(depfixtureDep)
	if !ok {
		t.Fatalf("Package(%s) missing", depfixtureDep)
	}
	if dep.Root {
		t.Fatalf("Package(%s).Root = true, want false (external module-cache dependency)", depfixtureDep)
	}
}
