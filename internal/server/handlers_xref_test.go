package server

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"go.lsp.dev/protocol"

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
