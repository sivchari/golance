package index

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sivchari/golance/internal/graph"
)

// testFilesInPackage returns the in-package _test.go files found in pkg.Dir
// alongside pkg.GoFiles — files internal/graph's loadMode (go/packages
// without Tests mode) never reports at all, so without this a unit's facts
// index has nothing to say about a definition, reference, or implementation
// query for a position inside one. Mirrors internal/check.Engine's
// resolveFiles/canonicalPackageName (in place since v0.1.7 for hover/
// completion/inlay hints in test files): a file only counts as "in package"
// if its own package clause matches pkg's canonical (non-test) package
// name, so the external "_test"-suffixed test package contributes nothing
// here either, exactly as it does not for check.Engine.
//
// File content is read through reader rather than os.ReadFile directly, so
// a Reindex run driven by an editor overlay (see FileReader's doc) sees a
// file's unsaved content instead of what is on disk — consistent with every
// other read processUnit performs through the same reader. Directory
// listing itself is always disk-based (os.ReadDir): a didSave notification
// — the only trigger for reindexing a single package — is only sent once
// its client has already written the file to disk, so a brand new
// _test.go file is already discoverable there by the time this runs.
//
// Any failure along the way (the directory cannot be listed, or pkg's own
// canonical package name cannot be determined) degrades to no test files at
// all rather than failing the whole unit: cross-reference features for the
// rest of the package are unaffected, only this run's test-file coverage is
// skipped.
func testFilesInPackage(pkg *graph.Package, reader FileReader) []string {
	name, ok := canonicalPackageName(pkg.GoFiles, reader)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(pkg.Dir)
	if err != nil {
		return nil
	}
	var tests []string
	for _, ent := range entries {
		if ent.IsDir() || !isTestGoFile(ent.Name()) {
			continue
		}
		path := filepath.Join(pkg.Dir, ent.Name())
		if pn, ok := packageClauseName(path, reader); ok && pn == name {
			tests = append(tests, path)
		}
	}
	sort.Strings(tests)
	return tests
}

// isTestGoFile reports whether base (a directory entry's base name) is a
// candidate in-package test file: the Go tool's own "_test.go" convention,
// excluding names it ignores entirely (a leading "." or "_").
func isTestGoFile(base string) bool {
	if !strings.HasSuffix(base, "_test.go") {
		return false
	}
	return !strings.HasPrefix(base, ".") && !strings.HasPrefix(base, "_")
}

// canonicalPackageName determines a package's declared Go package name from
// the first of goFiles (pkg's own known non-test files, which cannot be the
// external "_test" package) whose package clause reader can parse.
func canonicalPackageName(goFiles []string, reader FileReader) (string, bool) {
	for _, f := range goFiles {
		if name, ok := packageClauseName(f, reader); ok {
			return name, true
		}
	}
	return "", false
}

// packageClauseName reads path's package clause only (via reader), without
// parsing the rest of the file.
func packageClauseName(path string, reader FileReader) (string, bool) {
	src, err := reader(path)
	if err != nil {
		return "", false
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.PackageClauseOnly)
	if err != nil || f == nil {
		return "", false
	}
	return f.Name.Name, true
}

// effectiveGoFiles returns goFiles plus testFiles: the full file set
// processUnit indexes for one unit (pkg's own non-test files from the
// graph, plus its in-package _test.go files from testFilesInPackage).
func effectiveGoFiles(goFiles, testFiles []string) []string {
	if len(testFiles) == 0 {
		return goFiles
	}
	out := make([]string, 0, len(goFiles)+len(testFiles))
	out = append(out, goFiles...)
	out = append(out, testFiles...)
	return out
}
