package check

import (
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// loadTestSnapshot loads root's module restricted to pattern, e.g.
// "./basic/..." — narrower than newTestEngine's "./..." so a test can build
// two snapshots of the same module that each know about a disjoint subset of
// its packages.
func loadTestSnapshot(t *testing.T, root, pattern string) *graph.Snapshot {
	t.Helper()
	snap, err := graph.Load(graph.Options{Dir: root}, pattern)
	if err != nil {
		t.Fatalf("graph.Load(%q): %v", pattern, err)
	}
	return snap
}

// pkgPathForDir returns the pkgPath of snap's non-ForTest package whose Dir
// is dir, failing the test if there is none.
func pkgPathForDir(t *testing.T, snap *graph.Snapshot, dir string) string {
	t.Helper()
	for pkgPath, pkg := range snap.Packages {
		if pkg.ForTest == "" && pkg.Dir == dir {
			return pkgPath
		}
	}
	t.Fatalf("snapshot has no package with Dir = %q", dir)
	return ""
}

// TestGraphSource_Retarget_ReflectsNewSnapshot covers Retarget's contract: a
// GraphSource resolves PackageForFile against whichever snapshot it was most
// recently constructed with or Retarget-ed to, and a stale snapshot's
// packages are no longer resolvable afterward.
func TestGraphSource_Retarget_ReflectsNewSnapshot(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snapA := loadTestSnapshot(t, root, "./basic/...")
	snapB := loadTestSnapshot(t, root, "./lru/pkg1/...")

	basicFile := filepath.Join(root, "basic", "basic.go")
	basicDir := filepath.Dir(basicFile)
	pkg1File := filepath.Join(root, "lru", "pkg1", "pkg1.go")
	pkg1Dir := filepath.Dir(pkg1File)

	g := NewGraphSource(snapA, nil)

	pkgPath, dir, _, ok := g.PackageForFile(basicFile)
	if !ok {
		t.Fatalf("PackageForFile(basicFile) over snapA: ok = false, want true")
	}
	if want := pkgPathForDir(t, snapA, basicDir); pkgPath != want {
		t.Errorf("PackageForFile(basicFile) pkgPath = %q, want %q", pkgPath, want)
	}
	if dir != basicDir {
		t.Errorf("PackageForFile(basicFile) dir = %q, want %q", dir, basicDir)
	}
	if _, _, _, ok := g.PackageForFile(pkg1File); ok {
		t.Fatalf("PackageForFile(pkg1File) over snapA: ok = true, want false (snapA never loaded lru/pkg1)")
	}

	g.Retarget(snapB)

	if _, _, _, ok := g.PackageForFile(basicFile); ok {
		t.Errorf("PackageForFile(basicFile) after Retarget to snapB: ok = true, want false (snapB never loaded basic; Retarget must not still resolve against snapA)")
	}
	pkgPath, dir, _, ok = g.PackageForFile(pkg1File)
	if !ok {
		t.Fatalf("PackageForFile(pkg1File) after Retarget to snapB: ok = false, want true")
	}
	if want := pkgPathForDir(t, snapB, pkg1Dir); pkgPath != want {
		t.Errorf("PackageForFile(pkg1File) pkgPath = %q, want %q", pkgPath, want)
	}
	if dir != pkg1Dir {
		t.Errorf("PackageForFile(pkg1File) dir = %q, want %q", dir, pkg1Dir)
	}
}
