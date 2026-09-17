package index

import (
	"fmt"
	"go/token"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// cgoTestSourceReader returns a checkOnePackage readFile func backed by a
// fixed map, mirroring internal/depcheck/cgofile_test.go's own fixture
// shape without needing a real on-disk module.
func cgoTestSourceReader(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		src, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("not found: %s", path)
		}
		return []byte(src), nil
	}
}

// TestCheckOnePackage_SkipsCgoFiles pins internal/index's own counterpart
// to internal/depcheck's identical guard (see its
// TestProvider_Package_SkipsCgoFiles): a file importing the pseudo-package
// "C" cannot be checked without cgo preprocessing this indexer never runs,
// so it is skipped, losing only its own declarations while the rest of the
// package still indexes cleanly.
func TestCheckOnePackage_SkipsCgoFiles(t *testing.T) {
	const pkgPath = "example.com/cgomix"
	readFile := cgoTestSourceReader(map[string]string{
		"plain.go": "package cgomix\n\n// Plain is declared in a normal file.\nfunc Plain() int { return 1 }\n",
		"cgo.go":   "package cgomix\n\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\n\n// FromCgo leans on cgo preprocessing this checker never runs.\nfunc FromCgo() { C.free(nil) }\n",
	})

	fset := token.NewFileSet()
	imp := typecheck.NewImporter(fset, nil, nil, typecheck.NewCache())
	result, err := checkOnePackage(fset, imp, pkgPath, []string{"plain.go", "cgo.go"}, nil, readFile, "", false)
	if err != nil {
		t.Fatalf("checkOnePackage: %v", err)
	}
	if result.Incomplete {
		t.Errorf("result.Incomplete = true, want false (the cgo file must be skipped, not fail the check): FirstError=%q", result.FirstError)
	}
	if len(result.Export) == 0 {
		t.Error("result.Export is empty, want a produced export for the non-cgo declaration")
	}
}

// TestCheckOnePackage_OnlyCgoFilesDegradesCleanly pins the all-cgo edge for
// internal/index's own unit-processing path: with every file skipped,
// checkOnePackage must degrade to a clean Incomplete result naming pkgPath
// — not the "no parseable files" error a genuinely unreadable package
// gets, which would leave pkgPath with no index entry at all and cascade
// "has no recorded blob key" failures to every dependent (see
// checkOnePackage's own zero-files doc).
func TestCheckOnePackage_OnlyCgoFilesDegradesCleanly(t *testing.T) {
	const pkgPath = "example.com/onlycgo"
	readFile := cgoTestSourceReader(map[string]string{
		"only.go": "package onlycgo\n\nimport \"C\"\n\nfunc F() { C.free(nil) }\n",
	})

	fset := token.NewFileSet()
	imp := typecheck.NewImporter(fset, nil, nil, typecheck.NewCache())
	result, err := checkOnePackage(fset, imp, pkgPath, []string{"only.go"}, nil, readFile, "", false)
	if err != nil {
		t.Fatalf("checkOnePackage returned an error (want a graceful Incomplete degradation instead): %v", err)
	}
	if !result.Incomplete {
		t.Error("result.Incomplete = false, want true: every file was cgo, nothing was checked")
	}
	if result.FirstError == "" {
		t.Error("result.FirstError is empty, want a message naming the package and the cgo skip")
	}
	if len(result.Export) != 0 {
		t.Errorf("result.Export has %d byte(s), want none: nothing was checked", len(result.Export))
	}
	if len(result.Facts) == 0 {
		t.Error("result.Facts is empty, want a still-valid (if empty) facts blob so a dependent can at least resolve pkgPath's key")
	}
}
