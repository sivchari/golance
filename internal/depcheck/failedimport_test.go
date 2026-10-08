package depcheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

const (
	failedImportQ1Path      = "example.com/failedimport/q1"
	failedImportQ2Path      = "example.com/failedimport/q2"
	failedImportTopPath     = "example.com/failedimport/top"
	failedImportBlankPath   = "example.com/failedimport/blank"
	failedImportMissingPath = "example.com/failedimport/missing"
)

// failedImportMetadataSource reports four packages sharing one directory:
// "q1" and "q2", which both import the same path this source does not know
// at all; "top", which imports q1 and q2; and "blank", which imports the
// unknown path with a blank import only, so the unresolved import produces
// no type error of its own.
type failedImportMetadataSource struct {
	dir string
}

func (m failedImportMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	switch pkgPath {
	case failedImportQ1Path:
		return m.dir, []string{filepath.Join(m.dir, "q1.go")}, []string{failedImportMissingPath}, true
	case failedImportQ2Path:
		return m.dir, []string{filepath.Join(m.dir, "q2.go")}, []string{failedImportMissingPath}, true
	case failedImportTopPath:
		return m.dir, []string{filepath.Join(m.dir, "top.go")}, []string{failedImportQ1Path, failedImportQ2Path}, true
	case failedImportBlankPath:
		return m.dir, []string{filepath.Join(m.dir, "blank.go")}, []string{failedImportMissingPath}, true
	default:
		return "", nil, nil, false
	}
}

func writeFailedImportFixture(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"q1.go":    "package q1\n\nimport \"example.com/failedimport/missing\"\n\n// V references the unresolvable import.\nvar V = missing.V\n",
		"q2.go":    "package q2\n\nimport \"example.com/failedimport/missing\"\n\n// V references the unresolvable import.\nvar V = missing.V\n",
		"top.go":   "package top\n\nimport (\n\t_ \"example.com/failedimport/q1\"\n\t_ \"example.com/failedimport/q2\"\n)\n",
		"blank.go": "package blank\n\nimport _ \"example.com/failedimport/missing\"\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func newFailedImportProvider(t *testing.T) *Provider {
	t.Helper()
	dir := t.TempDir()
	writeFailedImportFixture(t, dir)
	return NewProvider(failedImportMetadataSource{dir: dir}, Options{})
}

// TestPackage_UnresolvableImportSharedWithinClosure is the regression test
// for the production "export data for X would reference two non-identical
// packages both named Y" failure (observed against
// github.com/snowflakedb/gosnowflake/v2 /
// golang.org/x/crypto/cryptobyte): go/types fabricates a DISTINCT fake
// *types.Package per failing Config.Check call for the same unresolvable
// import path, so two packages in one closure importing the same
// unresolvable path used to embed two non-identical packages with one
// PkgPath — exactly what typecheck.DuplicateImportPath exists to reject,
// turning internal/depexport's ExportData into a hard, persistent error.
// Every unresolvable import within one top-level closure must instead
// resolve to one shared placeholder instance.
func TestPackage_UnresolvableImportSharedWithinClosure(t *testing.T) {
	p := newFailedImportProvider(t)

	cp, err := p.Package(context.Background(), failedImportTopPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", failedImportTopPath, err)
	}
	if dup := typecheck.DuplicateImportPath(cp.Types()); dup != "" {
		t.Errorf("DuplicateImportPath = %q, want \"\" (one shared placeholder per unresolvable path per closure)", dup)
	}
	if !cp.Incomplete() {
		t.Error("Incomplete() = false for a closure containing an unresolvable import, want true")
	}
}

// TestPackage_FailedImportResultNotCached verifies that a package whose
// check could not resolve an import is never stored in the LRU: its
// placeholder import is closure-scoped, so a cached copy would leak that
// placeholder into later closures, where it is non-identical to both a
// fresh placeholder and any later real resolution of the same path —
// recreating the duplicate-import-path corruption from the other side.
func TestPackage_FailedImportResultNotCached(t *testing.T) {
	p := newFailedImportProvider(t)
	ctx := context.Background()

	for i := range 2 {
		cp, err := p.Package(ctx, failedImportQ1Path)
		if err != nil {
			t.Fatalf("Package(%s) call %d: %v", failedImportQ1Path, i+1, err)
		}
		if cp.Types() == nil {
			t.Fatalf("Types() = nil on call %d; a failed import must still degrade to a usable package", i+1)
		}
	}
	if got := p.Checked(); got != 2 {
		t.Errorf("Checked() = %d after two Package(%s) calls, want 2 (a failed-import result must not be served from the LRU)", got, failedImportQ1Path)
	}
}

// TestPackage_FailedImportPropagatesNoCache verifies the no-cache rule is
// transitive: a package that only REACHES an unresolvable import through
// its own dependency still embeds that dependency's closure-scoped
// placeholder, so caching it would leak the placeholder all the same.
func TestPackage_FailedImportPropagatesNoCache(t *testing.T) {
	p := newFailedImportProvider(t)
	ctx := context.Background()

	if _, err := p.Package(ctx, failedImportTopPath); err != nil {
		t.Fatalf("Package(%s) first call: %v", failedImportTopPath, err)
	}
	// top + q1 + q2: all three carry the placeholder, none may be cached.
	first := p.Checked()
	if _, err := p.Package(ctx, failedImportTopPath); err != nil {
		t.Fatalf("Package(%s) second call: %v", failedImportTopPath, err)
	}
	if got := p.Checked() - first; got != first {
		t.Errorf("second Package(%s) ran %d fresh checks, want %d (nothing in a placeholder-carrying closure may be cached)", failedImportTopPath, got, first)
	}
}

// TestPackage_BlankImportOfUnresolvablePathIsIncomplete pins the one case
// where no type error fires at all: a blank import of an unresolvable path
// references no symbol, so types.Config.Error never runs, and only the
// importer's own failed-import tracking can mark the result Incomplete —
// which internal/depexport relies on to refuse persisting it.
func TestPackage_BlankImportOfUnresolvablePathIsIncomplete(t *testing.T) {
	p := newFailedImportProvider(t)

	cp, err := p.Package(context.Background(), failedImportBlankPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", failedImportBlankPath, err)
	}
	if !cp.Incomplete() {
		t.Error("Incomplete() = false for a blank import of an unresolvable path, want true")
	}
}
