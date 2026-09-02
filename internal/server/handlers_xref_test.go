package server

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/rpc"
)

// TestResolverOrWarn_UsesLogMessageNotShowMessage verifies that the
// one-time notice resolverOrWarn sends while the index is still building
// goes out as window/logMessage, never window/showMessage: some clients
// (e.g. a terminal-based editor) render showMessage as a blocking modal the
// user must dismiss, which must not happen for a routine "still indexing"
// state — see logMessage's doc in indexer.go.
func TestResolverOrWarn_UsesLogMessageNotShowMessage(t *testing.T) {
	var out bytes.Buffer
	pr, pw := io.Pipe()
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))

	// Run Serve to completion (it returns as soon as its input hits EOF)
	// before calling resolverOrWarn: Serve's very first action is installing
	// its conn over out, and waiting for it to fully return — rather than
	// racing a live read loop — gives that write a clean happens-before
	// edge to this goroutine's later Notify calls, with no shared access
	// left concurrent by the time they run.
	done := make(chan struct{})
	go func() {
		_ = rpcServer.Serve(context.Background(), pr, &out)
		close(done)
	}()
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	<-done

	s := New(rpcServer, Options{Logger: newTestLogger(t)})

	if _, ok := s.resolverOrWarn(); ok {
		t.Fatal("resolverOrWarn() ok = true, want false: no index has been built yet")
	}

	written := out.String()
	if strings.Contains(written, protocol.MethodWindowShowMessage) {
		t.Errorf("resolverOrWarn() sent %s; want only %s while the index is building (showMessage can block on a modal in some editors): %q",
			protocol.MethodWindowShowMessage, protocol.MethodWindowLogMessage, written)
	}
	if !strings.Contains(written, protocol.MethodWindowLogMessage) {
		t.Errorf("resolverOrWarn() did not send %s: %q", protocol.MethodWindowLogMessage, written)
	}

	// A second call must not repeat the notice (see indexBuildingWarned).
	out.Reset()
	if _, ok := s.resolverOrWarn(); ok {
		t.Fatal("resolverOrWarn() second call ok = true, want false")
	}
	if out.Len() != 0 {
		t.Errorf("resolverOrWarn() second call sent %q, want nothing (one-time notice already sent)", out.String())
	}
}

// TestHandleRename_DegradesGracefullyOnResolverError checks that
// handleRename, given a position in a file resolver.Rename cannot resolve
// (here: a file outside any package the facts index knows about, the same
// "not part of any known package" condition resolveAt reports for a
// genuinely unresolvable rename target), degrades like its sibling handlers
// (Definition/References/Implementation/WorkspaceSymbol) instead of
// wrapping resolver.Rename's raw internal error text into an
// rpc.NewError InvalidRequest response.
func TestHandleRename_DegradesGracefullyOnResolverError(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "greet", "unsaved.go")
	openDoc(t, s, path, "package greet\n\nfunc Orphan() {}\n")

	result, err := s.handleRename(context.Background(), mustMarshal(t, &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     protocol.Position{Line: 2, Character: 5},
		},
		NewName: "Renamed",
	}))
	if err != nil {
		t.Fatalf("handleRename(unresolvable target) error = %v, want nil (a null result, not a wire error)", err)
	}
	if result != nil {
		t.Fatalf("handleRename(unresolvable target) result = %#v, want nil", result)
	}
}

// TestHandleRename_RefusesLoudlyOnDirtyBuffer is a regression test for the
// investigation's rename-corruption finding: correctResultRange's
// dirty-buffer correction (see dirty.go) only shifts line numbers via a
// naive top-down line diff and is blind to column-level edits on the same
// line. Here the buffer edit inserts characters before the second "Hello"
// occurrence on its own line, changing that occurrence's column without
// changing the file's line count at all — dirtyLineMap reports that line
// as needing no shift (ok=true, unchanged), so the stale on-disk column
// would silently be applied to the edited line's now-different content
// instead of being recognized as wrong. handleRename must refuse the whole
// rename with a clear error in this situation, never return a
// WorkspaceEdit that could land at the wrong column or silently omit the
// occurrence.
func TestHandleRename_RefusesLoudlyOnDirtyBuffer(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "greet", "greet.go")

	saved, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	openDoc(t, s, path, string(saved)) // matches disk: not dirty yet

	const from = `g := Hello("world")`
	const to = `x := 1; g := Hello("world")`
	dirty := strings.Replace(string(saved), from, to, 1)
	if dirty == string(saved) {
		t.Fatalf("test fixture: %q not found in %s", from, path)
	}
	changeDoc(t, s, path, 2, dirty)

	// Rename at Hello's declaration, on a line the edit above never
	// touched, so xrefPosition/correctQueryLine still resolves the right
	// symbol — the danger here is entirely in applying the resulting
	// edits, not in finding the rename target.
	pos := identPosition(t, path, 1) // 1st "Hello" occurrence: the declaration

	result, err := s.handleRename(context.Background(), mustMarshal(t, &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
		NewName: "Greet",
	}))
	if result != nil {
		t.Fatalf("handleRename(dirty buffer) result = %#v, want nil: never a WorkspaceEdit that could be silently wrong or partial", result)
	}
	if err == nil {
		t.Fatal("handleRename(dirty buffer) error = nil, want a loud error refusing the rename")
	}
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("handleRename(dirty buffer) error type = %T, want *rpc.Error", err)
	}
	if !strings.Contains(rpcErr.Message, "unsaved edits") {
		t.Errorf("handleRename(dirty buffer) error message = %q, want it to mention unsaved edits", rpcErr.Message)
	}
}

// TestHandleRename_AppliesEditsAcrossCleanBuffer checks that the dirty-file
// refusal added for TestHandleRename_RefusesLoudlyOnDirtyBuffer does not
// also block the ordinary case: a rename against a file with no unsaved
// changes (matching what the facts index was built from) still succeeds
// and edits every occurrence.
func TestHandleRename_AppliesEditsAcrossCleanBuffer(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "greet", "greet.go")

	saved, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	openDoc(t, s, path, string(saved)) // matches disk: not dirty

	pos := identPosition(t, path, 1) // Hello's declaration

	result, err := s.handleRename(context.Background(), mustMarshal(t, &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
		NewName: "Greet",
	}))
	if err != nil {
		t.Fatalf("handleRename(clean buffer) error = %v, want nil", err)
	}
	edit, ok := result.(*protocol.WorkspaceEdit)
	if !ok {
		t.Fatalf("handleRename(clean buffer) result type = %T, want *protocol.WorkspaceEdit", result)
	}
	edits, ok := edit.Changes[uri.File(path)]
	if !ok {
		t.Fatalf("handleRename(clean buffer) has no edits for %s: %#v", path, edit.Changes)
	}
	// greet.go's "Hello" occurs twice: the declaration and useHello's call.
	if len(edits) != 2 {
		t.Errorf("handleRename(clean buffer) edit count = %d, want 2 (declaration + call site)", len(edits))
	}
}

// TestHandleDefinition_Stdlib verifies dependencyDefinition's fallback: the
// workspace facts index only ever indexes root packages (see
// internal/index/scheduler.go's doc), so it has no answer for a standard
// library symbol used from a workspace file — handleDefinition must resolve
// it instead through the type-checked package's own Uses/Defs and the
// shared dependency importer's export-data position, landing inside GOROOT.
func TestHandleDefinition_Stdlib(t *testing.T) {
	s, snap, _ := newTestServer(t)
	pkg, ok := snap.Packages["example.com/servermod/depuse"]
	if !ok || len(pkg.GoFiles) == 0 {
		t.Fatal("depuse package not found in test workspace")
	}
	file := pkg.GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	pos := identPositionIn(t, file, data, "Sprintf", 1)

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(fmt.Sprintf): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(fmt.Sprintf): result = %#v, want a single location", result)
	}
	target := locs[0].URI.FsPath()
	if !strings.HasSuffix(target, filepath.FromSlash("fmt/print.go")) {
		t.Errorf("definition file = %q, want it to end with fmt/print.go (inside GOROOT)", target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("definition file %s does not exist on disk: %v", target, err)
	}
	if locs[0].Range.Start.Line == 0 {
		t.Error("definition line = 0, want a real declaration line inside fmt/print.go")
	}
}

// importPathPosition parses path's content and returns the LSP Position of
// a byte inside importPath's quoted string in its import spec, the
// counterpart of identPositionIn for a query position that is never on an
// *ast.Ident.
func importPathPosition(t *testing.T, path string, data []byte, importPath string) protocol.Position {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, imp := range f.Imports {
		unquoted, err := strconv.Unquote(imp.Path.Value)
		if err != nil || unquoted != importPath {
			continue
		}
		tf := fset.File(imp.Path.Pos())
		offset := tf.Offset(imp.Path.Pos()) + 1 // inside the opening quote
		pos, ok := overlay.UTF16PositionForByteOffset(data, offset)
		if !ok {
			t.Fatalf("%s: offset %d out of range", path, offset)
		}
		return pos
	}
	t.Fatalf("%s: import %q not found", path, importPath)
	return protocol.Position{}
}

// TestHandleDefinition_ImportPath_Workspace covers "Go to Definition" on an
// import spec's path string naming a different workspace (root) package:
// facts extraction never indexes an *ast.ImportSpec (there is no
// types.Object use/def for one), so this always falls through to
// definitionFallback's importDefinition step, never the primary facts-index
// path — unlike an ordinary cross-package identifier reference, which
// resolver.Definition already answers directly.
func TestHandleDefinition_ImportPath_Workspace(t *testing.T) {
	s, snap, _ := newTestServer(t)
	pkg, ok := snap.Packages["example.com/servermod/depuse"]
	if !ok || len(pkg.GoFiles) == 0 {
		t.Fatal("depuse package not found in test workspace")
	}
	file := pkg.GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	pos := importPathPosition(t, file, data, "example.com/servermod/greet")

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(import \"example.com/servermod/greet\"): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(import greet): result = %#v, want a single location", result)
	}
	greetFile := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	if got := locs[0].URI.FsPath(); got != greetFile {
		t.Errorf("definition file = %q, want %q (greet's own package file)", got, greetFile)
	}
	greetSrc, err := os.ReadFile(filepath.Clean(greetFile))
	if err != nil {
		t.Fatalf("read %s: %v", greetFile, err)
	}
	wantLine := -1
	for i, line := range strings.Split(string(greetSrc), "\n") {
		if strings.HasPrefix(line, "package greet") {
			wantLine = i
			break
		}
	}
	if wantLine < 0 {
		t.Fatalf("test fixture: no package clause line found in %s", greetFile)
	}
	if int(locs[0].Range.Start.Line) != wantLine {
		t.Errorf("definition line = %d, want %d (greet's package clause)", locs[0].Range.Start.Line, wantLine)
	}
}

// TestHandleDefinition_ImportPath_Stdlib covers "Go to Definition" on an
// import spec's path string naming a standard library package: unlike a
// stdlib identifier reference (TestHandleDefinition_Stdlib, resolved
// through the shared dependency importer's export data), this never
// type-checks anything — internal/graph's Snapshot already has fmt's own
// Go files from packages.Load's NeedFiles (see internal/graph's loadMode),
// covering the whole transitive import graph, not just root packages.
func TestHandleDefinition_ImportPath_Stdlib(t *testing.T) {
	s, snap, _ := newTestServer(t)
	pkg, ok := snap.Packages["example.com/servermod/depuse"]
	if !ok || len(pkg.GoFiles) == 0 {
		t.Fatal("depuse package not found in test workspace")
	}
	file := pkg.GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	pos := importPathPosition(t, file, data, "fmt")

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(import \"fmt\"): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(import fmt): result = %#v, want a single location", result)
	}
	fmtPkg, ok := snap.Packages["fmt"]
	if !ok || len(fmtPkg.GoFiles) == 0 {
		t.Fatal("fmt package not found in the loaded graph")
	}
	if got, want := locs[0].URI.FsPath(), fmtPkg.GoFiles[0]; got != want {
		t.Errorf("definition file = %q, want %q (fmt's first Go file, inside GOROOT)", got, want)
	}
	if _, err := os.Stat(locs[0].URI.FsPath()); err != nil {
		t.Errorf("definition file %s does not exist on disk: %v", locs[0].URI.FsPath(), err)
	}
}

// TestHandleDefinition_NoIndex_SamePackage is a regression test for a real
// user report: a second editor window on a large repo can hit
// textDocument/definition while another golance session holds the
// per-root index's lock (or the index is still building on a cold start),
// which previously made every definition query — including same-package
// jumps that need no index at all — return an empty result instead of
// falling back to definitionFallback's type-info-based path. Against a
// server with no index (see newTestServerNoIndex), a same-package jump
// must still land at the exact declaration position, column included: this
// case never touches export data (see langfeat.SamePackageDefinition), so
// nothing here degrades.
func TestHandleDefinition_NoIndex_SamePackage(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	pos := identPosition(t, file, 2) // call site in useHello

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(no index, same package): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(no index, same package): result = %#v, want a single location", result)
	}
	declPos := identPosition(t, file, 1) // func Hello(...) declaration
	if locs[0].Range.Start != declPos {
		t.Fatalf("definition start = %+v, want %+v (exact column, no index needed)", locs[0].Range.Start, declPos)
	}
}

// TestHandleDefinition_NoIndex_Stdlib is TestHandleDefinition_Stdlib's
// no-index counterpart: a standard library symbol must still resolve
// through dependencyDefinition's export-data path (langfeat.DependencyDefinition)
// when the facts index is entirely unavailable, landing inside GOROOT.
func TestHandleDefinition_NoIndex_Stdlib(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	pkg, ok := snap.Packages["example.com/servermod/depuse"]
	if !ok || len(pkg.GoFiles) == 0 {
		t.Fatal("depuse package not found in test workspace")
	}
	file := pkg.GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	pos := identPositionIn(t, file, data, "Sprintf", 1)

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(no index, fmt.Sprintf): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(no index, fmt.Sprintf): result = %#v, want a single location", result)
	}
	target := locs[0].URI.FsPath()
	if !strings.HasSuffix(target, filepath.FromSlash("fmt/print.go")) {
		t.Errorf("definition file = %q, want it to end with fmt/print.go (inside GOROOT)", target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("definition file %s does not exist on disk: %v", target, err)
	}
	if locs[0].Range.Start.Line == 0 {
		t.Error("definition line = 0, want a real declaration line inside fmt/print.go")
	}
}

// TestHandleDefinition_NoIndex_OtherWorkspacePackage is a regression guard
// for a real hazard TestE2E_WorktreeSharesIndex caught: dependencyDefinition
// must keep declining to answer for a different *workspace* (root) package
// even from definitionFallback (the index-unavailable path), never just
// from the index-error path. A second session (or a cold-start session
// before its own index build finishes) treats a non-empty
// textDocument/definition result as a signal the index is now usable, and
// handleDidSave silently drops the reindex for any edit saved while the
// index is still unavailable (no retry once it later opens) — so answering
// this case via possibly-premature export data would let that race succeed
// on stale grounds, exactly the failure TestE2E_WorktreeSharesIndex
// reproduced. See dependencyDefinition's doc for the full mechanism.
func TestHandleDefinition_NoIndex_OtherWorkspacePackage(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	depusePkg, ok := snap.Packages["example.com/servermod/depuse"]
	if !ok || len(depusePkg.GoFiles) == 0 {
		t.Fatal("depuse package not found in test workspace")
	}
	depuseFile := depusePkg.GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(depuseFile))
	if err != nil {
		t.Fatalf("read %s: %v", depuseFile, err)
	}
	pos := identPositionIn(t, depuseFile, data, "Greeting", 1) // greet.Greeting reference

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(depuseFile)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(no index, greet.Greeting): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 0 {
		t.Fatalf("handleDefinition(no index, greet.Greeting): result = %#v, want an empty result (never a stale root-package answer)", result)
	}
}

// TestHandleDefinition_WorkspaceSymbolPreferred is a regression guard for
// dependencyDefinition's fallback: a definition query on a cross-package
// workspace symbol must still be answered by the workspace facts index
// (accurate to the exact declaration column) rather than falling through to
// the export-data fallback (which always degrades to column 1 — see
// dependencyDefinition's doc).
func TestHandleDefinition_WorkspaceSymbolPreferred(t *testing.T) {
	s, snap, _ := newTestServer(t)
	depusePkg, ok := snap.Packages["example.com/servermod/depuse"]
	if !ok || len(depusePkg.GoFiles) == 0 {
		t.Fatal("depuse package not found in test workspace")
	}
	depuseFile := depusePkg.GoFiles[0]
	data, err := os.ReadFile(filepath.Clean(depuseFile))
	if err != nil {
		t.Fatalf("read %s: %v", depuseFile, err)
	}
	pos := identPositionIn(t, depuseFile, data, "Greeting", 1) // greet.Greeting reference

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(depuseFile)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(greet.Greeting): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(greet.Greeting): result = %#v, want a single location", result)
	}

	greetFile := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	if got := locs[0].URI.FsPath(); got != greetFile {
		t.Fatalf("definition file = %q, want %q (greet.go, via the workspace facts index)", got, greetFile)
	}
	greetText, err := os.ReadFile(filepath.Clean(greetFile))
	if err != nil {
		t.Fatalf("read %s: %v", greetFile, err)
	}
	want := identPositionIn(t, greetFile, greetText, "Greeting", 1) // type Greeting struct declaration
	if locs[0].Range.Start != want {
		t.Errorf("definition start = %+v, want %+v (Greeting's declaration, exact column from the facts index)", locs[0].Range.Start, want)
	}
}

// TestHandleDefinition_Builtin covers "go to definition" on a universe
// (predeclared) identifier — the workspace facts index never has an entry
// for one (nothing declares it anywhere in the workspace), so this pins
// handleDefinition's fallthrough into definitionFallback's new
// builtinDefinition step landing inside the toolchain's own
// $GOROOT/src/builtin/builtin.go, even with a fully built index available.
func TestHandleDefinition_Builtin(t *testing.T) {
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

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(len): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(len): result = %#v, want a single location", result)
	}
	target := locs[0].URI.FsPath()
	if !strings.HasSuffix(filepath.ToSlash(target), "builtin/builtin.go") {
		t.Errorf("definition file = %q, want it to end with builtin/builtin.go (inside GOROOT)", target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("definition file %s does not exist on disk: %v", target, err)
	}
}
