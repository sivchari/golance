package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

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

	saved, err := os.ReadFile(path)
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
	rpcErr, ok := err.(*rpc.Error)
	if !ok {
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

	saved, err := os.ReadFile(path)
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
