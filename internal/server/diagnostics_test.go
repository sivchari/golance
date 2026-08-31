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

// TestNotifyDiagnostics_SetsVersionForOpenFile covers the second half of
// Finding 5's fix: a published textDocument/publishDiagnostics notification
// must carry the document's current version (LSP 3.15+) whenever the file
// is open, so a client can discard/reconcile an out-of-order publish
// against the version it currently has instead of trusting it blindly.
func TestNotifyDiagnostics_SetsVersionForOpenFile(t *testing.T) {
	var out bytes.Buffer
	pr, pw := io.Pipe()
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))

	// Run Serve to completion first (see
	// TestResolverOrWarn_UsesLogMessageNotShowMessage's identical pattern in
	// handlers_xref_test.go): its conn is installed as Serve's very first
	// action, and waiting for the whole call to finish before calling
	// notifyDiagnostics gives that write a clean happens-before edge, with
	// nothing left concurrent by the time it runs.
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
	file := filepath.Join(t.TempDir(), "a.go")
	s.overlay.DidOpen(&protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri.File(file), Version: 7, Text: "package a\n"},
	})

	s.notifyDiagnostics(file, nil)

	written := out.String()
	if !strings.Contains(written, `"version":7`) {
		t.Fatalf("notifyDiagnostics(open file at version 7) did not publish version 7: %q", written)
	}
}

// TestNotifyDiagnostics_OmitsVersionForClosedFile verifies that a file with
// no tracked overlay (e.g. closed in the narrow window between
// publishDiagnostics deciding to notify it and this call — see
// notifyDiagnostics's doc) does not get a fabricated version.
func TestNotifyDiagnostics_OmitsVersionForClosedFile(t *testing.T) {
	var out bytes.Buffer
	pr, pw := io.Pipe()
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))

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
	file := filepath.Join(t.TempDir(), "a.go")

	s.notifyDiagnostics(file, nil)

	written := out.String()
	if strings.Contains(written, `"version"`) {
		t.Fatalf("notifyDiagnostics(never-opened file) published a version: %q", written)
	}
}
