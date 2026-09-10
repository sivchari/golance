package depcheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const (
	incompleteBrokenPkgPath  = "example.com/incompletefixture/broken"
	incompleteCleanPkgPath   = "example.com/incompletefixture/clean"
	incompleteOuterPkgPath   = "example.com/incompletefixture/outer"
	incompleteMissingPkgPath = "example.com/incompletefixture/missing"
)

// incompleteFixtureMetadataSource reports three packages sharing one
// directory: "broken", which imports an import path this source does not
// know at all (standing in for a transiently-unresolvable transitive
// dependency); "clean", which imports nothing; and "outer", which imports
// "broken" but has no unresolved import of its own — for exercising
// CheckedPackage.Incomplete's direct-error and transitive-propagation
// cases independently.
type incompleteFixtureMetadataSource struct {
	dir string
}

func (m incompleteFixtureMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	switch pkgPath {
	case incompleteBrokenPkgPath:
		return m.dir, []string{filepath.Join(m.dir, "broken.go")}, []string{incompleteMissingPkgPath}, true
	case incompleteCleanPkgPath:
		return m.dir, []string{filepath.Join(m.dir, "clean.go")}, nil, true
	case incompleteOuterPkgPath:
		return m.dir, []string{filepath.Join(m.dir, "outer.go")}, []string{incompleteBrokenPkgPath}, true
	default:
		return "", nil, nil, false
	}
}

func writeIncompleteFixture(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"broken.go": "package broken\n\nimport \"example.com/incompletefixture/missing\"\n\n// V references the unresolvable import.\nvar V = missing.V\n",
		"clean.go":  "package clean\n\n// V is an ordinary exported value.\nvar V = 1\n",
		"outer.go":  "package outer\n\nimport _ \"example.com/incompletefixture/broken\"\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// TestCheckedPackage_Incomplete_UnresolvedImport verifies H9's core
// mechanism: checking a package whose own import cannot be resolved (the
// transient-unresolvable-dependency scenario H9 exists for) reports
// Incomplete() == true, even though check's own best-effort Error handling
// still returns a usable CheckedPackage rather than failing outright.
func TestCheckedPackage_Incomplete_UnresolvedImport(t *testing.T) {
	dir := t.TempDir()
	writeIncompleteFixture(t, dir)
	meta := incompleteFixtureMetadataSource{dir: dir}
	p := NewProvider(meta, Options{})

	cp, err := p.Package(context.Background(), incompleteBrokenPkgPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", incompleteBrokenPkgPath, err)
	}
	if !cp.Incomplete() {
		t.Error("Incomplete() = false for a package with an unresolvable import, want true")
	}
	if cp.Types() == nil {
		t.Error("Types() = nil; check should still return a usable, if degraded, *types.Package")
	}
}

// TestCheckedPackage_Incomplete_CleanImportIsFalse is the control: a
// package with no unresolvable imports must report Incomplete() == false.
func TestCheckedPackage_Incomplete_CleanImportIsFalse(t *testing.T) {
	dir := t.TempDir()
	writeIncompleteFixture(t, dir)
	meta := incompleteFixtureMetadataSource{dir: dir}
	p := NewProvider(meta, Options{})

	cp, err := p.Package(context.Background(), incompleteCleanPkgPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", incompleteCleanPkgPath, err)
	}
	if cp.Incomplete() {
		t.Error("Incomplete() = true for a package with no unresolvable imports, want false")
	}
}

// TestCheckedPackage_Incomplete_PropagatesTransitively verifies that
// importing an Incomplete package makes the importer Incomplete too, even
// when the importer's own files trigger no type error of their own: without
// this propagation (see ctxImporter.importIncomplete), "outer" would report
// Incomplete() == false purely because go/types does not reliably re-invoke
// Config.Error just for referencing an already-degraded import.
func TestCheckedPackage_Incomplete_PropagatesTransitively(t *testing.T) {
	dir := t.TempDir()
	writeIncompleteFixture(t, dir)
	meta := incompleteFixtureMetadataSource{dir: dir}
	p := NewProvider(meta, Options{})

	cp, err := p.Package(context.Background(), incompleteOuterPkgPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", incompleteOuterPkgPath, err)
	}
	if !cp.Incomplete() {
		t.Error("Incomplete() = false for a package importing an Incomplete dependency, want true (transitive propagation)")
	}
}
