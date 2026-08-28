package check

import (
	"context"
	"go/token"
	"go/types"
	"path/filepath"
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
	src := NewGraphSource(snap)
	imp := func() types.ImporterFrom {
		return typecheck.NewImporter(depFset, nil, snap, depCache)
	}

	ov := overlay.New()
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
