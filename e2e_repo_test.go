package golance_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
)

// e2eLocs records the exact 0-based positions the E2E subtests query,
// captured while the synthetic module is written so nothing is re-parsed at
// query time.
type e2eLocs struct {
	utilFile string            // lib/util/util.go
	utilSrc  string            // lib/util/util.go's original source, for the unsaved-edit hover subtest
	sumDecl  protocol.Position // "Sum" in "func Sum(a, b int) int"

	appFile      string            // app/app.go, imports lib/util and lib/store
	sumCallInApp protocol.Position // "Sum" in the util.Sum call inside app.go

	extraFile string // extra/extra.go, a second importer of lib/util

	usepkgFile  string            // usepkg/usepkg.go, imports lib/store
	selectorPos protocol.Position // just after "s." in "s.Get()", for selector completion

	storeFile    string            // lib/store/store.go
	storeGetDecl protocol.Position // "Get" in "func (s *Store) Get() string"

	brokenFile string // broken/broken.go, a package with a type error
}

// writeE2EModule writes a single-module synthetic workspace into a temp dir:
//
//	lib/util    Sum, called by app and extra
//	lib/store   Store, whose method set drives the selector completion subtest
//	app         imports lib/util and lib/store
//	extra       imports lib/util only (a second cross-package reference)
//	usepkg      imports lib/store, for the completion subtest
//	broken      a standalone package with a type error, for the diagnostics subtest
//	empty       only an external "_test" package, so go/packages reports it
//	            with zero GoFiles — regression coverage for the indexer
//	            treating that as skipped rather than a fatal build error
//
// The returned root is symlink-resolved because the resolved path (macOS:
// /var -> /private/var) must match what the server reports back in
// definition/references locations.
func writeE2EModule(t *testing.T) (string, e2eLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eLocs

	writeE2EFile(t, root, "go.mod", "module example.com/e2e\n\ngo 1.23\n")

	const utilSrc = `package util

// Sum adds two ints.
func Sum(a, b int) int {
	return a + b
}
`
	locs.utilFile = writeE2EFile(t, root, "lib/util/util.go", utilSrc)
	locs.utilSrc = utilSrc
	locs.sumDecl = mustPos(t, utilSrc, "func Sum", "Sum")

	const storeSrc = `package store

// Store is a simple key-value holder.
type Store struct {
	Value string
}

// Get returns the stored value.
func (s *Store) Get() string {
	return s.Value
}

// Zulu, Alpha, and Mike exist only to give Store's method set a declaration
// order that differs from alphabetical order, regression coverage for
// cross-package method reference identity.
func (s *Store) Zulu() string {
	return s.Value
}

func (s *Store) Alpha() string {
	return s.Value
}

func (s *Store) Mike() string {
	return s.Value
}
`
	locs.storeFile = writeE2EFile(t, root, "lib/store/store.go", storeSrc)
	locs.storeGetDecl = mustPos(t, storeSrc, "func (s *Store) Get", "Get")

	const appSrc = `package app

import (
	"example.com/e2e/lib/store"
	"example.com/e2e/lib/util"
)

// Compute calls util.Sum.
func Compute() int {
	return util.Sum(1, 2)
}

// New returns a fresh Store.
func New() *store.Store {
	return &store.Store{}
}

// Describe calls the Store method set across the package boundary.
func Describe() string {
	return New().Get()
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)
	locs.sumCallInApp = mustPos(t, appSrc, "util.Sum(", "Sum")

	const extraSrc = `package extra

import "example.com/e2e/lib/util"

// Double doubles n using util.Sum.
func Double(n int) int {
	return util.Sum(n, n)
}
`
	locs.extraFile = writeE2EFile(t, root, "extra/extra.go", extraSrc)

	const usepkgSrc = `package usepkg

import "example.com/e2e/lib/store"

// Use exercises Store's method set for a selector completion query.
func Use() string {
	var s store.Store
	return s.Get()
}
`
	locs.usepkgFile = writeE2EFile(t, root, "usepkg/usepkg.go", usepkgSrc)
	getCall := mustPos(t, usepkgSrc, "s.Get()", "Get")
	locs.selectorPos = getCall // cursor lands right before "Get", i.e. just after "s."

	const brokenSrc = `package broken

// Broken has a type error: a string literal cannot be returned as int.
func Broken() int {
	return "not an int"
}
`
	locs.brokenFile = writeE2EFile(t, root, "broken/broken.go", brokenSrc)

	const emptySrc = `package empty_test

import "testing"

// TestNothing exists only so go/packages reports this directory as a
// package with zero GoFiles (no non-test source at all).
func TestNothing(t *testing.T) {}
`
	writeE2EFile(t, root, "empty/e_test.go", emptySrc)

	return root, locs
}

// writeHeavyPackage writes a synthetic package named pkgName under root
// with numFiles files, each declaring a handful of trivial functions, so
// that a real parse/type-check of the whole package (forced whenever any
// one of its files changes, since Go compiles a package as a unit) takes
// long enough to dominate e2e process/IPC overhead — the property
// TestE2E_BranchSwitchNoRetypecheck needs for a reliable timing
// comparison between "real re-type-check" and "CAS hit, no type-check at
// all". It returns the path of the package's first file (a safe edit
// target: editing any one file forces the whole package to be rechecked)
// together with that file's own content, so a caller that wants to edit it
// doesn't need to read it back from disk.
func writeHeavyPackage(t *testing.T, root, pkgName string, numFiles int) (firstFile, firstFileSrc string) {
	t.Helper()
	for i := 0; i < numFiles; i++ {
		var b strings.Builder
		_, _ = fmt.Fprintf(&b, "package %s\n\n", pkgName)
		for j := 0; j < 8; j++ {
			_, _ = fmt.Fprintf(&b, "// F%d_%d does a little arithmetic.\nfunc F%d_%d(x int) int {\n\treturn x*%d + %d\n}\n\n", i, j, i, j, i+1, j)
		}
		src := b.String()
		path := writeE2EFile(t, root, fmt.Sprintf("%s/f%d.go", pkgName, i), src)
		if i == 0 {
			firstFile, firstFileSrc = path, src
		}
	}
	return firstFile, firstFileSrc
}

func writeE2EFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

// mustPos returns the 0-based LSP position of token on the first line of
// content containing lineSubstr. All generated sources are ASCII, so byte
// columns equal the UTF-16 columns LSP expects.
func mustPos(t *testing.T, content, lineSubstr, token string) protocol.Position {
	t.Helper()
	for i, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, lineSubstr) {
			continue
		}
		col := strings.Index(line, token)
		if col < 0 {
			// The explicit returns give static analysis the early-exit edges
			// t.Fatalf's runtime.Goexit does not.
			t.Fatalf("token %q not found on line %q", token, line)
			return protocol.Position{}
		}
		if col > math.MaxUint32 {
			t.Fatalf("column %d exceeds uint32", col)
			return protocol.Position{}
		}
		return protocol.Position{Line: uint32(i), Character: uint32(col)}
	}
	t.Fatalf("no line contains %q", lineSubstr)
	return protocol.Position{}
}
