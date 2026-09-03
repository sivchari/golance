package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// depfixturePkg and depfixtureDep are the two packages
// testdata/depfixture's own module graph contains: the Root package itself,
// and its one real external (module-cache) dependency.
const (
	depfixturePkg = "example.com/depfixture"
	depfixtureDep = "golang.org/x/sync/singleflight"
)

// loadDepFixture loads testdata/depfixture — a minimal module with one real
// external module-cache dependency (see its own go.mod) — the fixture
// TestLoad_RootPackagesHaveNoExportFile and
// TestLoad_DependencyExportFileResolvedWithoutMainNeedExportFile use to
// verify loadDepExportFiles' split-loading behavior end to end.
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

// TestLoad_RootPackagesHaveNoExportFile verifies that Load never populates
// ExportFile for a Root package: golance always type-checks a Root package
// straight from source (see internal/index/scheduler.go's doc and
// internal/check), so requesting -export compilation for it — the cost
// loadMode's own doc describes as the dominant cold-worktree-open latency
// through v0.5.0 — buys nothing.
func TestLoad_RootPackagesHaveNoExportFile(t *testing.T) {
	snap := loadDepFixture(t)

	pkg, ok := snap.Package(depfixturePkg)
	if !ok {
		t.Fatalf("Package(%s) missing", depfixturePkg)
	}
	if !pkg.Root {
		t.Fatalf("Package(%s).Root = false, want true", depfixturePkg)
	}
	if pkg.ExportFile != "" {
		t.Errorf("Root package %s has ExportFile = %q, want empty (never requested)", depfixturePkg, pkg.ExportFile)
	}
}

// TestLoad_DependencyExportFileResolvedWithoutMainNeedExportFile verifies
// that a real, non-root module-cache dependency still ends up with a valid,
// readable ExportFile after Load — even though the main "./..." load no
// longer requests packages.NeedExportFile at all (see loadMode's doc) —
// because loadDepExportFiles' own separate, batched load resolves it. This
// is the behavior that replaces compiling -export data for the entire
// workspace with compiling it only for the (already GOCACHE-warm, since
// module-cache paths are stable across worktrees) dependency subset that
// internal/typecheck.ExportFileSource actually needs.
func TestLoad_DependencyExportFileResolvedWithoutMainNeedExportFile(t *testing.T) {
	snap := loadDepFixture(t)

	pkg, ok := snap.Package(depfixtureDep)
	if !ok {
		t.Fatalf("Package(%s) missing", depfixtureDep)
	}
	if pkg.Root {
		t.Fatalf("Package(%s).Root = true, want false (external module-cache dependency)", depfixtureDep)
	}
	if pkg.ExportFile == "" {
		t.Fatalf("Package(%s).ExportFile is empty, want a real GOCACHE export file", depfixtureDep)
	}
	if _, err := os.Stat(pkg.ExportFile); err != nil {
		t.Errorf("stat ExportFile %s: %v", pkg.ExportFile, err)
	}

	// ExportFile itself (Snapshot.ExportFile, the accessor
	// internal/typecheck.ExportFileSource actually calls) must resolve the
	// same file directly, without falling back to reloadExportFile's own
	// recovery subprocess.
	file, ok := snap.ExportFile(depfixtureDep)
	if !ok {
		t.Fatalf("Snapshot.ExportFile(%s) = not ok, want the file loadDepExportFiles already resolved", depfixtureDep)
	}
	if file != pkg.ExportFile {
		t.Errorf("Snapshot.ExportFile(%s) = %s, want %s", depfixtureDep, file, pkg.ExportFile)
	}
}
