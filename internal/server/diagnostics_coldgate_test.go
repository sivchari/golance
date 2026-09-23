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

// TestPublishDiagnostics_ColdGateOnlyDiagPublishesNothing pins the
// unused-import regression this suite fixes: go/types never reports
// "imported and not used" for an import it failed to resolve in the first
// place (confirmed via a standalone types.Config.Check run against a
// failing importer -- it reports only the import failure), so a file whose
// sole diagnostic this recheck found is cold-gate-suppressed must not be
// reported as clean. Before the fix, publishDiagnostics treated a file with
// no diagnostics left after filtering exactly like a genuinely clean file
// and sent an explicit empty publishDiagnostics notification for it,
// permanently hiding the real "imported and not used" diagnostic the next,
// index-ready recheck would otherwise have reported -- since
// waitForDiagnostics-style callers act on the first notification for a URI,
// not the first accurate one.
func TestPublishDiagnostics_ColdGateOnlyDiagPublishesNothing(t *testing.T) {
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
	file := filepath.Join(t.TempDir(), "unusedimport.go")
	s.overlay.DidOpen(&protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri.File(file), Version: 1, Text: "package unusedimport\n\nimport \"fmt\"\n"},
	})
	// s.idx is left nil: the facts index is not ready, so "fmt" itself is
	// gated -- go/types then reports only the import failure, never the
	// unused-import diagnostic a fully resolved import would have produced.

	res := &check.Result{
		PkgPath: "example.com/unusedimport",
		Dir:     filepath.Dir(file),
		Files:   []string{file},
		Diags: []check.Diag{
			{
				File: file, StartLine: 2, StartCol: 0, EndLine: 2, EndCol: 12,
				Message:  "could not import fmt (" + coldGateMissMarker + ": fmt)",
				Severity: check.SeverityError,
			},
		},
	}
	s.publishDiagnostics(res)

	if written := out.String(); written != "" {
		t.Errorf("publishDiagnostics sent a notification for a file whose only diagnostic was cold-gate-suppressed, want none until a real recheck: %s", written)
	}
}
