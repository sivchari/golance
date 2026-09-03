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

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
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

// checkIndexUnavailableError asserts err is the distinct
// LSPErrorCodesRequestFailed error indexUnavailableError returns — never an
// ordinary nil (empty-result) error — so a caller (e.g. an automated
// client) can tell "the index is not available yet" apart from a genuine
// "0 matches" answer.
func checkIndexUnavailableError(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: error = nil, want a distinct index-unavailable error", name)
	}
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s: error type = %T, want *rpc.Error", name, err)
	}
	if rpcErr.Code != int32(protocol.LSPErrorCodesRequestFailed) {
		t.Errorf("%s: error code = %d, want %d (LSPErrorCodesRequestFailed)", name, rpcErr.Code, protocol.LSPErrorCodesRequestFailed)
	}
}

// TestIndexUnavailable_ReturnsDistinctErrorNotEmptyResult pins the
// unavailable-vs-empty contract for references/implementation/
// workspaceSymbol/rename: while the facts index has not finished building
// (s.idx nil — the same state a still-running cold-start buildIndex, or a
// failed tryWarmOpen, leaves it in; see newTestServerNoIndex), each of
// these must answer with indexUnavailableError's distinct error rather than
// an ordinary empty result. A field report traced an automated client
// misreading an empty references result as "this symbol is unused," when
// the real cause was querying mid-build — see indexUnavailableError's doc.
// handleDefinition is deliberately excluded: it keeps answering through its
// own fallback chain (definitionFallback), which serves useful results
// without the index at all.
func TestIndexUnavailable_ReturnsDistinctErrorNotEmptyResult(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	pos := identPosition(t, file, 1) // Hello's declaration

	t.Run("references", func(t *testing.T) {
		result, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		checkIndexUnavailableError(t, "references", err)
		if result != nil {
			t.Errorf("references: result = %#v, want nil", result)
		}
	})

	t.Run("implementation", func(t *testing.T) {
		result, err := s.handleImplementation(context.Background(), mustMarshal(t, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		checkIndexUnavailableError(t, "implementation", err)
		if result != nil {
			t.Errorf("implementation: result = %#v, want nil", result)
		}
	})

	t.Run("workspaceSymbol", func(t *testing.T) {
		result, err := s.handleWorkspaceSymbol(context.Background(), mustMarshal(t, &protocol.WorkspaceSymbolParams{Query: "Hello"}))
		checkIndexUnavailableError(t, "workspaceSymbol", err)
		if result != nil {
			t.Errorf("workspaceSymbol: result = %#v, want nil", result)
		}
	})

	t.Run("rename", func(t *testing.T) {
		result, err := s.handleRename(context.Background(), mustMarshal(t, &protocol.RenameParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
			NewName: "Greeting2",
		}))
		checkIndexUnavailableError(t, "rename", err)
		if result != nil {
			t.Errorf("rename: result = %#v, want nil", result)
		}
	})

	t.Run("definition_keeps_fallback", func(t *testing.T) {
		result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     identPosition(t, file, 2), // useHello's call site, resolvable via SamePackageDefinition
			},
		}))
		if err != nil {
			t.Fatalf("handleDefinition: error = %v, want nil (definitionFallback answers without the index)", err)
		}
		locs, ok := result.(protocol.LocationSlice)
		if !ok || len(locs) == 0 {
			t.Fatalf("handleDefinition: result = %#v, want at least one location from definitionFallback", result)
		}
	})
}

// TestReferences_TransitionsFromIndexUnavailableToResults exercises the
// full unavailable -> ready transition against one Server: the very same
// query at the very same position must switch from
// indexUnavailableError's distinct error to a real answer once the facts
// index is installed via s.idx.Store — the same action
// openIndexAfterBuild takes on a successful build — pinning that the new
// error path only ever reflects "no index yet," not a permanent
// degradation for that position.
func TestReferences_TransitionsFromIndexUnavailableToResults(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	pos := identPosition(t, file, 1) // Hello's declaration

	params := mustMarshal(t, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	})

	if _, err := s.handleReferences(context.Background(), params); err == nil {
		t.Fatal("handleReferences before index ready: error = nil, want indexUnavailableError")
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
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, false)})

	result, err := s.handleReferences(context.Background(), params)
	if err != nil {
		t.Fatalf("handleReferences after index ready: %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) == 0 {
		t.Fatalf("handleReferences after index ready: result = %#v, want at least one location", result)
	}
}

// TestWorkspaceSymbol_TransitionsFromIndexUnavailableToResults is
// TestReferences_TransitionsFromIndexUnavailableToResults's counterpart for
// workspace/symbol.
func TestWorkspaceSymbol_TransitionsFromIndexUnavailableToResults(t *testing.T) {
	s, snap := newTestServerNoIndex(t)

	params := mustMarshal(t, &protocol.WorkspaceSymbolParams{Query: "Hello"})

	if _, err := s.handleWorkspaceSymbol(context.Background(), params); err == nil {
		t.Fatal("handleWorkspaceSymbol before index ready: error = nil, want indexUnavailableError")
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
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, false)})

	result, err := s.handleWorkspaceSymbol(context.Background(), params)
	if err != nil {
		t.Fatalf("handleWorkspaceSymbol after index ready: %v", err)
	}
	syms, ok := result.(protocol.SymbolInformationSlice)
	if !ok || len(syms) == 0 {
		t.Fatalf("handleWorkspaceSymbol after index ready: result = %#v, want at least one symbol", result)
	}
}

// TestIndexUnavailableError_DistinguishesFailedFromBuilding pins
// indexUnavailableError's wording: it must say the index failed to build,
// not that it is "still building" (which would wrongly suggest a retry is
// already under way and will eventually succeed on its own), once
// warnIndexUnavailable has recorded a build attempt that left no index
// open at all (s.indexFailedWarned) — see its own doc.
func TestIndexUnavailableError_DistinguishesFailedFromBuilding(t *testing.T) {
	s, _ := newTestServerNoIndex(t)

	if err := s.indexUnavailableError("references"); strings.Contains(err.Error(), "failed") {
		t.Errorf("indexUnavailableError() before any failure = %q, want it to describe an in-flight build, not a failure", err.Error())
	} else if !strings.Contains(err.Error(), "still building") {
		t.Errorf("indexUnavailableError() before any failure = %q, want it to mention the build is still in progress", err.Error())
	}

	s.indexFailedWarned.Store(true)

	err := s.indexUnavailableError("references")
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("indexUnavailableError() after indexFailedWarned = %q, want it to describe a failed build, not a routine still-building state", err.Error())
	}
	if strings.Contains(err.Error(), "still building") {
		t.Errorf("indexUnavailableError() after indexFailedWarned = %q, want it not to claim the index is still building", err.Error())
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

// TestHandleTypeDefinition_Builtin covers item 1(a): typeDefinition on an
// identifier whose static type is a predeclared basic type (here, Count's
// []int parameter v, whose element type int is predeclared) resolves into
// builtin.go via handleTypeDefinition's typeDefinitionBuiltin path, the
// same target TestHandleDefinition_Builtin above pins for plain "Go to
// Definition" -- before this fix, TypeDefinition returned (nil, nil) for
// every predeclared type, so this request answered no locations at all.
func TestHandleTypeDefinition_Builtin(t *testing.T) {
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
	pos := identPositionIn(t, file, data, "v", 1) // Count(v []int)'s own parameter

	result, err := s.handleTypeDefinition(context.Background(), mustMarshal(t, &protocol.TypeDefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleTypeDefinition(v): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleTypeDefinition(v): result = %#v, want a single location", result)
	}
	target := locs[0].URI.FsPath()
	if !strings.HasSuffix(filepath.ToSlash(target), "builtin/builtin.go") {
		t.Errorf("type definition file = %q, want it to end with builtin/builtin.go (inside GOROOT)", target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("type definition file %s does not exist on disk: %v", target, err)
	}
}
