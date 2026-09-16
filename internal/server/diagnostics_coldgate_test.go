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

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/rpc"
)

// TestPublishDiagnostics_SuppressesColdGateImportDiag pins B1: while the
// facts index is not ready (s.idx nil), publishDiagnostics must drop a
// diagnostic caused by depCacheHolder.importer's cold-build gate (see
// coldGateMissMarker/isColdGateImportDiag) but keep a genuine type error in
// the same file.
func TestPublishDiagnostics_SuppressesColdGateImportDiag(t *testing.T) {
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
	s.overlay.DidOpen(&protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri.File(file), Version: 1, Text: "package a\n"},
	})
	// s.idx is left nil: the facts index is not ready.

	res := &check.Result{
		PkgPath: "example.com/coldgatediag/a",
		Dir:     filepath.Dir(file),
		Files:   []string{file},
		Diags: []check.Diag{
			{
				File: file, StartLine: 1, StartCol: 0, EndLine: 1, EndCol: 10,
				Message:  "could not import example.com/coldgatediag/b (" + coldGateMissMarker + ": example.com/coldgatediag/b)",
				Severity: check.SeverityError,
			},
			{
				File: file, StartLine: 2, StartCol: 0, EndLine: 2, EndCol: 5,
				Message:  "undefined: Foo",
				Severity: check.SeverityError,
			},
		},
	}
	s.publishDiagnostics(res)

	written := out.String()
	if strings.Contains(written, coldGateMissMarker) {
		t.Errorf("published diagnostics contain the cold-gate marker, want it suppressed while the index is not ready: %s", written)
	}
	if !strings.Contains(written, "undefined: Foo") {
		t.Errorf("published diagnostics do not contain the genuine type error, want it kept: %s", written)
	}
}

// TestPublishDiagnostics_KeepsColdGateImportDiagOnceIndexReady is the
// guardrail half of TestPublishDiagnostics_SuppressesColdGateImportDiag:
// once s.idx is installed (index ready), the identical diagnostic text
// must publish normally -- isColdGateImportDiag's filter is scoped to the
// cold-build window, never a general-purpose message suppression, so a
// package that is STILL unresolvable once the index is ready must keep
// surfacing it.
func TestPublishDiagnostics_KeepsColdGateImportDiagOnceIndexReady(t *testing.T) {
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
	s.overlay.DidOpen(&protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri.File(file), Version: 1, Text: "package a\n"},
	})
	s.idx.Store(&indexState{}) // index ready (fields unused by publishDiagnostics)

	res := &check.Result{
		PkgPath: "example.com/coldgatediag/a",
		Dir:     filepath.Dir(file),
		Files:   []string{file},
		Diags: []check.Diag{
			{
				File: file, StartLine: 1, StartCol: 0, EndLine: 1, EndCol: 10,
				Message:  "could not import example.com/coldgatediag/b (" + coldGateMissMarker + ": example.com/coldgatediag/b)",
				Severity: check.SeverityError,
			},
		},
	}
	s.publishDiagnostics(res)

	written := out.String()
	if !strings.Contains(written, coldGateMissMarker) {
		t.Errorf("published diagnostics dropped the import-failure diagnostic once the index is ready, want it kept: %s", written)
	}
}
