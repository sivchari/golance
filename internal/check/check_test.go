package check

import (
	"context"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestEngine_Get_TypesInfo covers (a): Get returns a *types.Info in which a
// known symbol resolves to the expected type.
func TestEngine_Get_TypesInfo(t *testing.T) {
	e, root := newTestEngine(t, overlay.New(), Options{})
	path := filepath.Join(root, "basic", "basic.go")

	cp, err := e.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	obj := cp.Package().Scope().Lookup("Add")
	if obj == nil {
		t.Fatal("Add not found in package scope")
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		t.Fatalf("Add type = %T, want *types.Signature", obj.Type())
	}
	if sig.Params().Len() != 2 || sig.Results().Len() != 1 {
		t.Errorf("Add signature = %s, want (int, int) int", sig)
	}
	if sig.Results().At(0).Type().String() != "int" {
		t.Errorf("Add result type = %s, want int", sig.Results().At(0).Type())
	}
}

// TestEngine_Get_OverlayContent covers (b): unsaved overlay content is
// reflected in the check result, not just the on-disk content.
func TestEngine_Get_OverlayContent(t *testing.T) {
	ov := overlay.New()
	e, root := newTestEngine(t, ov, Options{})
	path := filepath.Join(root, "overlaypkg", "overlaypkg.go")

	ov.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI:        uri.File(path),
		LanguageID: "go",
		Version:    1,
		Text:       "package overlaypkg\n\nfunc Foo() int { return 1 }\n\nfunc Bar() int { return 2 }\n",
	}})

	cp, err := e.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cp.Package().Scope().Lookup("Bar") == nil {
		t.Error("Bar (present only in the overlay) not found in package scope")
	}
}

// TestEngine_Get_SyntaxErrorPartialAST covers (c): a syntax error in one
// declaration still yields a partial AST (with the rest of the package
// intact) plus a parse diagnostic, rather than a hard Get error.
func TestEngine_Get_SyntaxErrorPartialAST(t *testing.T) {
	e, root := newTestEngine(t, overlay.New(), Options{})
	path := filepath.Join(root, "broken", "broken.go")

	cp, err := e.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(cp.Files()) == 0 {
		t.Fatal("expected a partial AST even with a syntax error")
	}
	if cp.Package().Scope().Lookup("Good") == nil {
		t.Error("Good should still be checkable despite Bad's syntax error")
	}

	diags := Diagnostics(cp, e.reader)
	if len(diags) == 0 {
		t.Fatal("expected at least one parse diagnostic")
	}
	if diags[0].Severity != SeverityError {
		t.Errorf("parse diagnostic severity = %v, want SeverityError", diags[0].Severity)
	}
}

// TestEngine_Get_AdhocPackage covers Phase 3 of design-adhoc-packages.md:
// testdata/module/testdata/fixture is a directory graph.Load never reports
// (the go tool always excludes "testdata" directories from "./..."
// patterns), so GraphSource.PackageForFile falls all the way through to
// its ad-hoc synthesis. Get on one of its files must still succeed,
// joining same-package-clause siblings (a_fixture.go and b_fixture.go)
// while excluding a different-clause sibling (z_other.go), tag the result
// with an "adhoc:"-prefixed pkgPath, and surface an unresolvable import
// (c_badimport.go) as an ordinary diagnostic rather than an error from Get
// itself.
func TestEngine_Get_AdhocPackage(t *testing.T) {
	e, root := newTestEngine(t, overlay.New(), Options{})
	path := filepath.Join(root, "testdata", "fixture", "a_fixture.go")

	cp, err := e.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !strings.HasPrefix(cp.PkgPath(), "adhoc:") {
		t.Errorf(`PkgPath() = %q, want an "adhoc:"-prefixed pkgPath`, cp.PkgPath())
	}
	if cp.Dir() != filepath.Dir(path) {
		t.Errorf("Dir() = %q, want %q", cp.Dir(), filepath.Dir(path))
	}

	scope := cp.Package().Scope()
	if scope.Lookup("Foo") == nil {
		t.Error("Foo (a_fixture.go) not found in the ad-hoc package's scope")
	}
	if scope.Lookup("Bar") == nil {
		t.Error("Bar (b_fixture.go, a same-package-clause sibling) not found in the ad-hoc package's scope")
	}
	if scope.Lookup("Baz") != nil {
		t.Error("Baz (z_other.go, a different package clause) must be excluded from the ad-hoc unit, but was found in scope")
	}

	diags := Diagnostics(cp, e.reader)
	if len(diags) == 0 {
		t.Fatal("want at least one diagnostic for c_badimport.go's unresolvable import, got none")
	}
}

// TestEngine_LRUEvictionAndFocus covers (f): non-focused packages are
// evicted LRU-first once the cache is at capacity, while the focused
// package is exempt.
func TestEngine_LRUEvictionAndFocus(t *testing.T) {
	e, root := newTestEngine(t, overlay.New(), Options{MaxLRU: 2})
	pkgFile := func(name string) string {
		return filepath.Join(root, "lru", name, name+".go")
	}

	ctx := context.Background()
	if _, err := e.Get(ctx, pkgFile("pkg1")); err != nil {
		t.Fatalf("Get pkg1: %v", err)
	}
	e.SetFocus(pkgFile("pkg1"))

	if _, err := e.Get(ctx, pkgFile("pkg2")); err != nil {
		t.Fatalf("Get pkg2: %v", err)
	}
	if _, err := e.Get(ctx, pkgFile("pkg3")); err != nil {
		t.Fatalf("Get pkg3: %v", err)
	}

	e.mu.Lock()
	_, hasPkg1 := e.cache[filepath.Dir(pkgFile("pkg1"))]
	_, hasPkg2 := e.cache[filepath.Dir(pkgFile("pkg2"))]
	_, hasPkg3 := e.cache[filepath.Dir(pkgFile("pkg3"))]
	size := len(e.cache)
	e.mu.Unlock()

	if !hasPkg1 {
		t.Error("focused pkg1 should not be evicted")
	}
	if hasPkg2 {
		t.Error("pkg2 should have been evicted (least recently used, non-focus)")
	}
	if !hasPkg3 {
		t.Error("pkg3 should be cached (most recently checked)")
	}
	if size != 2 {
		t.Errorf("cache size = %d, want 2 (MaxLRU)", size)
	}
}

// TestEngine_DependencyDecodeCachedAcrossRechecks covers the persistent
// dependency cache (plan-feat-v0.1.md's check/typecheck cache sharing): an
// Importer built once and reused across rechecks (as production wiring in
// internal/server does, and as this test's imp closure does by capturing a
// single depFset/depCache pair) must decode a given dependency's export
// data at most once, even though the package importing it is rechecked
// multiple times with different content.
func TestEngine_DependencyDecodeCachedAcrossRechecks(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	depFset := token.NewFileSet()
	depCache := typecheck.NewCache()
	ov := overlay.New()
	src := NewGraphSource(snap, ov)
	imp := func() types.ImporterFrom {
		return typecheck.NewImporter(depFset, nil, snap, depCache)
	}

	e := New(src, ov, imp, Options{})
	path := filepath.Join(root, "depuser", "depuser.go")

	ctx := context.Background()
	if _, err := e.Get(ctx, path); err != nil {
		t.Fatalf("Get (1st check): %v", err)
	}
	firstDecodes := depCache.Decodes()
	if firstDecodes == 0 {
		t.Fatal("expected at least one decode after the first check (fmt's export data)")
	}

	// Force a real recheck: change the overlay content (a different
	// contentHash) rather than calling Invalidate, so Get does synchronous
	// work instead of racing a debounce timer.
	ov.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI:        uri.File(path),
		LanguageID: "go",
		Version:    1,
		Text:       "package depuser\n\nimport \"fmt\"\n\nfunc Greet(name string) string {\n\treturn fmt.Sprintf(\"hi, %s\", name)\n}\n",
	}})
	if _, err := e.Get(ctx, path); err != nil {
		t.Fatalf("Get (2nd check): %v", err)
	}
	secondDecodes := depCache.Decodes()

	if secondDecodes != firstDecodes {
		t.Errorf("Decodes after 2nd recheck = %d, want unchanged from %d (fmt should be served from the persistent cache, not re-decoded)", secondDecodes, firstDecodes)
	}
}

// openOverlayOnly opens path in ov with text, without ever writing it to
// disk — simulating a brand-new, never-saved file an editor just created.
func openOverlayOnly(ov *overlay.Overlay, path, text string) {
	ov.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI:        uri.File(path),
		LanguageID: "go",
		Version:    1,
		Text:       text,
	}})
}

// TestEngine_Get_UnsavedNewFileJoinsKnownPackage covers Phase 1's core
// case: a new .go file created in an editor, in a directory that is
// already a known package, joins that package and is type-checked as part
// of it even though it was never saved to disk.
func TestEngine_Get_UnsavedNewFileJoinsKnownPackage(t *testing.T) {
	ov := overlay.New()
	e, root := newTestEngine(t, ov, Options{})
	newPath := filepath.Join(root, "overlaypkg", "newfile.go")
	openOverlayOnly(ov, newPath, "package overlaypkg\n\nfunc Bar() int { return 2 }\n")

	cp, err := e.Get(context.Background(), newPath)
	if err != nil {
		t.Fatalf("Get(new unsaved file): %v", err)
	}
	if cp.Package().Scope().Lookup("Bar") == nil {
		t.Error("Bar, declared only in the unsaved new file, not found in package scope")
	}
	if cp.Package().Scope().Lookup("Foo") == nil {
		t.Error("Foo, declared in the pre-existing on-disk file, not found in package scope")
	}
	if text, ok := cp.FileText(newPath); !ok || len(text) == 0 {
		t.Errorf("FileText(%s) = (%q, %v), want the overlay content and ok=true", newPath, text, ok)
	}
}

// TestEngine_Get_UnsavedNewFileWrongPackageClauseExcluded covers the
// complementary edge case: an unsaved new file in a known package's
// directory whose package clause names a different package is excluded
// from that package's file set, exactly as an on-disk file with a
// mismatched package clause already is.
func TestEngine_Get_UnsavedNewFileWrongPackageClauseExcluded(t *testing.T) {
	ov := overlay.New()
	e, root := newTestEngine(t, ov, Options{})
	wrongPath := filepath.Join(root, "overlaypkg", "wrongclause.go")
	openOverlayOnly(ov, wrongPath, "package wrong\n\nfunc Baz() int { return 3 }\n")

	basicPath := filepath.Join(root, "overlaypkg", "overlaypkg.go")
	cp, err := e.Get(context.Background(), basicPath)
	if err != nil {
		t.Fatalf("Get(overlaypkg.go): %v", err)
	}
	if cp.Package().Scope().Lookup("Foo") == nil {
		t.Error("Foo should still be checkable despite the mismatched-package sibling file")
	}
	if cp.Package().Scope().Lookup("Baz") != nil {
		t.Error("Baz, declared in a file with a mismatched package clause, should not join the package")
	}
	if _, ok := cp.FileText(wrongPath); ok {
		t.Errorf("FileText(%s) ok = true, want false: the mismatched-clause file should not be part of the checked package", wrongPath)
	}
}

// TestEngine_Get_UnknownDirectoryStillFails covers the scope guard: a file
// in a directory SnapshotSource has never heard of (no ad-hoc synthesis, in
// or out of the overlay) still degrades to "not found," exactly as before
// Phase 1.
func TestEngine_Get_UnknownDirectoryStillFails(t *testing.T) {
	ov := overlay.New()
	e, root := newTestEngine(t, ov, Options{})
	unknownPath := filepath.Join(root, "totally-unknown-dir", "standalone.go")
	openOverlayOnly(ov, unknownPath, "package standalone\n\nfunc Qux() int { return 4 }\n")

	if _, err := e.Get(context.Background(), unknownPath); err == nil {
		t.Fatal("Get(file in an unknown directory) succeeded, want an error")
	}
}

// TestEngine_Get_InPackageTestFileJoinsPackage covers the second symptom
// the directory fallback fixes: an on-disk in-package "_test.go" file is
// never in the import graph's GoFiles (packages.Load without Tests:true
// omits test files from a package's GoFiles entirely, see
// internal/graph.loadMode), so GraphSource's exact-path lookup alone never
// resolves it. The directory fallback makes it resolve to the enclosing
// package, letting check.Engine.resolveFiles's existing same-package-name
// filtering fold it into the checked unit like any other sibling file.
func TestEngine_Get_InPackageTestFileJoinsPackage(t *testing.T) {
	e, root := newTestEngine(t, overlay.New(), Options{})
	testFile := filepath.Join(root, "withtests", "withtests_test.go")

	cp, err := e.Get(context.Background(), testFile)
	if err != nil {
		t.Fatalf("Get(in-package _test.go file): %v", err)
	}
	if cp.Package().Scope().Lookup("TestOnlyHelper") == nil {
		t.Error("TestOnlyHelper, declared only in the in-package _test.go file, not found in package scope")
	}
	if cp.Package().Scope().Lookup("Value") == nil {
		t.Error("Value, declared in the base file, not found in package scope")
	}
	if _, ok := cp.FileText(testFile); !ok {
		t.Errorf("FileText(%s) ok = false, want true: the in-package test file should be part of the checked package", testFile)
	}
}

// TestEngine_Get_ExternalTestPackageFileExcluded pins the complementary
// scope guard: a "package foo_test" external test file in the same
// directory still degrades to no-result for that file — external test
// packages are Phase 2, not this change — even though the directory
// fallback now resolves its path to the base package too.
func TestEngine_Get_ExternalTestPackageFileExcluded(t *testing.T) {
	e, root := newTestEngine(t, overlay.New(), Options{})
	baseFile := filepath.Join(root, "withtests", "withtests.go")
	extFile := filepath.Join(root, "withtests", "withtests_ext_test.go")

	cp, err := e.Get(context.Background(), baseFile)
	if err != nil {
		t.Fatalf("Get(withtests.go): %v", err)
	}
	if cp.Package().Scope().Lookup("ExternalOnly") != nil {
		t.Error("ExternalOnly, declared in the external \"_test\" package file, should not join the base package")
	}
	if _, ok := cp.FileText(extFile); ok {
		t.Errorf("FileText(%s) ok = true, want false: the external test package file should not be part of the checked package", extFile)
	}
}
