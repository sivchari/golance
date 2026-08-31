package server

import (
	"bytes"
	"context"
	"io"
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
