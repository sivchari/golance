package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// testLogWriter forwards log output to t.Logf so it appears alongside the
// test's own output only when the test fails or -v is set.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func newTestLogger(t *testing.T) *log.Logger {
	return log.New(testLogWriter{t}, "", 0)
}

// newTestServer builds a Server wired over testdata/module, with its
// workspace and facts index populated exactly as handleInitialize would —
// but without going through handleInitialize itself, so tests never
// trigger the real indexer subprocess (see internal/server/indexer.go).
// index.Build runs in-process here, the same way internal/index's and
// internal/xref's own tests build a facts database.
func newTestServer(t *testing.T) (*Server, *graph.Snapshot, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if _, err := index.Build(context.Background(), snap, db, cas, index.Options{}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, false)})

	return s, snap, root
}

// newTestServerNoIndex builds a Server wired over testdata/module exactly
// like newTestServer, except s.idx is left at its zero value (nil): the
// same state a failed tryWarmOpen (e.g. store.Open timing out because
// another golance session holds the per-root index's lock) or a
// still-running buildIndex on a cold start leaves it in — s.idx is only
// ever Store'd on success (see lifecycle.go, buildIndexLocked).
// resolverOrWarn reports ok=false against this server, the same as it
// would against a real server whose facts index is unavailable.
func newTestServerNoIndex(t *testing.T) (*Server, *graph.Snapshot) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)

	return s, snap
}

// identPosition parses path's on-disk content and returns the LSP Position
// of the occurrence-th (1-based) identifier named "Hello", in source order.
func identPosition(t *testing.T, path string, occurrence int) protocol.Position {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return identPositionIn(t, path, data, "Hello", occurrence)
}

// identPositionIn parses data as path's content (without touching disk)
// and returns the LSP Position of the occurrence-th (1-based) identifier
// named name, in source order. Used to locate a position in unsaved
// editor-buffer content.
func identPositionIn(t *testing.T, path string, data []byte, name string, occurrence int) protocol.Position {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var positions []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			positions = append(positions, id.Pos())
		}
		return true
	})
	if occurrence < 1 || occurrence > len(positions) {
		t.Fatalf("%s: found %d occurrences of %q, want at least %d", path, len(positions), name, occurrence)
	}
	tf := fset.File(positions[occurrence-1])
	offset := tf.Offset(positions[occurrence-1])
	pos, ok := overlay.UTF16PositionForByteOffset(data, offset)
	if !ok {
		t.Fatalf("%s: offset %d out of range", path, offset)
	}
	return pos
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := protocol.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestHandleHover(t *testing.T) {
	s, snap, _ := newTestServer(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	pos := identPosition(t, file, 2) // call site in useHello

	result, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleHover: %v", err)
	}
	hover, ok := result.(*protocol.Hover)
	if !ok || hover == nil {
		t.Fatalf("handleHover: result = %#v, want *protocol.Hover", result)
	}
	md, ok := hover.Contents.(*protocol.MarkupContent)
	if !ok {
		t.Fatalf("hover.Contents = %#v, want *protocol.MarkupContent", hover.Contents)
	}
	if want := "func Hello"; !contains(md.Value, want) {
		t.Fatalf("hover content = %q, want it to contain %q", md.Value, want)
	}
}

// TestHandleHover_Builtin covers hover on a universe (predeclared)
// identifier: unlike a same-package or cross-package object, its
// types.Object has no Pkg() at all, so this pins Hover's new builtin
// branch (langfeat.hoverBuiltin) resolving into the toolchain's own
// builtin.go declaration and doc comment through the full handleHover
// request path.
func TestHandleHover_Builtin(t *testing.T) {
	s, snap, _ := newTestServer(t)
	pkg, ok := snap.Packages["example.com/servermod/builtinuse"]
	if !ok || len(pkg.GoFiles) == 0 {
		t.Fatal("builtinuse package not found in test workspace")
	}
	file := pkg.GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	pos := identPositionIn(t, file, data, "len", 1) // len(v) call site

	result, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleHover(len): %v", err)
	}
	hover, ok := result.(*protocol.Hover)
	if !ok || hover == nil {
		t.Fatalf("handleHover(len): result = %#v, want *protocol.Hover", result)
	}
	md, ok := hover.Contents.(*protocol.MarkupContent)
	if !ok {
		t.Fatalf("hover.Contents = %#v, want *protocol.MarkupContent", hover.Contents)
	}
	if want := "func len(v Type) int"; !contains(md.Value, want) {
		t.Fatalf("hover content = %q, want it to contain %q", md.Value, want)
	}
	if want := "The len built-in function returns the length of v"; !contains(md.Value, want) {
		t.Fatalf("hover content = %q, want it to contain %q (builtin.go's doc comment)", md.Value, want)
	}
}

func TestHandleDefinition(t *testing.T) {
	s, snap, _ := newTestServer(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	pos := identPosition(t, file, 2) // call site in useHello

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition: %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition: result = %#v, want a single location", result)
	}
	declPos := identPosition(t, file, 1) // func Hello(...) declaration
	if locs[0].Range.Start != declPos {
		t.Fatalf("definition start = %+v, want %+v", locs[0].Range.Start, declPos)
	}
}

func TestHandleReferences(t *testing.T) {
	s, snap, _ := newTestServer(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	pos := identPosition(t, file, 1) // declaration

	result, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}))
	if err != nil {
		t.Fatalf("handleReferences: %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleReferences: result = %#v, want a single reference", result)
	}
	callPos := identPosition(t, file, 2)
	if locs[0].Range.Start != callPos {
		t.Fatalf("reference start = %+v, want %+v", locs[0].Range.Start, callPos)
	}
}

func TestHandleDocumentSymbol(t *testing.T) {
	s, snap, _ := newTestServer(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]

	result, err := s.handleDocumentSymbol(context.Background(), mustMarshal(t, &protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
	}))
	if err != nil {
		t.Fatalf("handleDocumentSymbol: %v", err)
	}
	syms, ok := result.(protocol.DocumentSymbolSlice)
	if !ok {
		t.Fatalf("handleDocumentSymbol: result = %#v, want protocol.DocumentSymbolSlice", result)
	}
	var names []string
	for _, s := range syms {
		names = append(names, s.Name)
	}
	if !containsStr(names, "Greeting") || !containsStr(names, "Hello") || !containsStr(names, "useHello") {
		t.Fatalf("document symbols = %v, want Greeting, Hello, useHello", names)
	}
}

// TestHandleFoldingRange_UnknownPackageReturnsEmptyResult checks the same
// degradation as handlers_langfeat_test.go's hover/completion cases for
// handleFoldingRange, which — unlike hover/completion — calls
// ws.engine.Get directly rather than through the checkedFile helper.
func TestHandleFoldingRange_UnknownPackageReturnsEmptyResult(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "greet", "unsaved.go")

	result, err := s.handleFoldingRange(context.Background(), mustMarshal(t, &protocol.FoldingRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
	}))
	if err != nil {
		t.Fatalf("handleFoldingRange(unknown package) error = %v, want nil (an empty result, not a wire error)", err)
	}
	ranges, ok := result.([]protocol.FoldingRange)
	if !ok || len(ranges) != 0 {
		t.Fatalf("handleFoldingRange(unknown package) result = %#v, want an empty []protocol.FoldingRange", result)
	}
}

// TestHandleSelectionRange_UnknownPackageReturnsEmptyResult checks the same
// degradation as TestHandleFoldingRange_UnknownPackageReturnsEmptyResult for
// handleSelectionRange, which also calls ws.engine.Get directly.
func TestHandleSelectionRange_UnknownPackageReturnsEmptyResult(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "greet", "unsaved.go")

	result, err := s.handleSelectionRange(context.Background(), mustMarshal(t, &protocol.SelectionRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
		Positions:    []protocol.Position{{Line: 0, Character: 0}},
	}))
	if err != nil {
		t.Fatalf("handleSelectionRange(unknown package) error = %v, want nil (an empty result, not a wire error)", err)
	}
	ranges, ok := result.([]protocol.SelectionRange)
	if !ok || len(ranges) != 0 {
		t.Fatalf("handleSelectionRange(unknown package) result = %#v, want an empty []protocol.SelectionRange", result)
	}
}

func TestHandleFormatting(t *testing.T) {
	s, _, root := newTestServer(t)
	file := filepath.Join(root, "greet", "greet.go")

	result, err := s.handleFormatting(context.Background(), mustMarshal(t, &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
	}))
	if err != nil {
		t.Fatalf("handleFormatting: %v", err)
	}
	edits, ok := result.([]protocol.TextEdit)
	if !ok {
		t.Fatalf("handleFormatting: result = %#v, want []protocol.TextEdit", result)
	}
	// greet.go is already gofmt-clean, so formatting it should be a no-op.
	if len(edits) != 0 {
		t.Fatalf("handleFormatting: got %d edits for an already-formatted file, want 0", len(edits))
	}
}

func TestHandleWorkspaceSymbol(t *testing.T) {
	s, _, _ := newTestServer(t)

	result, err := s.handleWorkspaceSymbol(context.Background(), mustMarshal(t, &protocol.WorkspaceSymbolParams{Query: "Hello"}))
	if err != nil {
		t.Fatalf("handleWorkspaceSymbol: %v", err)
	}
	syms, ok := result.(protocol.SymbolInformationSlice)
	if !ok || len(syms) == 0 {
		t.Fatalf("handleWorkspaceSymbol: result = %#v, want at least one symbol", result)
	}
	if syms[0].Name != "Hello" {
		t.Fatalf("handleWorkspaceSymbol: name = %q, want %q", syms[0].Name, "Hello")
	}
}

// TestDidOpenDidChangeReflectedInHover checks that an unsaved edit made
// through the didOpen/didChange notification handlers is immediately
// visible to a subsequent hover — i.e. that overlay content, not just
// on-disk content, drives interactive queries.
func TestDidOpenDidChangeReflectedInHover(t *testing.T) {
	s, snap, _ := newTestServer(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	disk, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	if err := s.handleDidOpen(context.Background(), mustMarshal(t, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri.File(file), Version: 1, Text: string(disk)},
	})); err != nil {
		t.Fatalf("handleDidOpen: %v", err)
	}

	edited := string(disk) + "\n// Renamed is a second alias for documentation purposes.\nfunc Renamed(name string) Greeting { return Hello(name) }\n"
	if err := s.handleDidChange(context.Background(), mustMarshal(t, &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Version:                2,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: edited},
		},
	})); err != nil {
		t.Fatalf("handleDidChange: %v", err)
	}

	got, err := s.overlay.ReadFile(file)
	if err != nil {
		t.Fatalf("overlay.ReadFile: %v", err)
	}
	if string(got) != edited {
		t.Fatalf("overlay content after didChange = %q, want %q", got, edited)
	}

	pos := identPositionIn(t, file, []byte(edited), "Renamed", 1)
	result, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleHover (after edit): %v", err)
	}
	if _, ok := result.(*protocol.Hover); !ok {
		t.Fatalf("handleHover (after edit): result = %#v, want a hover for the newly-added Renamed func", result)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
