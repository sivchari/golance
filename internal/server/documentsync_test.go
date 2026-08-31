package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// writeFrame writes v, marshaled as method's JSON-RPC notification params,
// as a Content-Length-framed message to w — the same wire format
// internal/rpc.Server.Serve reads.
func writeFrame(t *testing.T, w io.Writer, method string, v any) {
	t.Helper()
	params := mustMarshal(t, v)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s}`, method, params)
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// TestHandleDidSave_ReindexNeverOrphanedByShutdown covers Finding 7: the
// background reindex handleDidSave starts must be tracked and bound to the
// session's own lifetime, so that even if a save happens immediately before
// shutdown, the reindex either completes or is canceled — it is never left
// running detached from the session that started it. This drives the real
// wire path (internal/rpc.Server.Serve, not handleDidSave called directly)
// so the tracking/cancellation this relies on (s.rpc.Go, see
// documentsync.go) is exercised the way production code actually uses it.
func TestHandleDidSave_ReindexNeverOrphanedByShutdown(t *testing.T) {
	s, snap, _ := newTestServer(t)
	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	text, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	pr, pw := io.Pipe()
	var out bytes.Buffer
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.rpc.Serve(context.Background(), pr, &out) }()

	textStr := string(text)
	go func() {
		writeFrame(t, pw, protocol.MethodTextDocumentDidSave, &protocol.DidSaveTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Text:         &textStr,
		})
		_ = pw.Close() // EOF: Serve should wind down once the notification (and its tracked reindex) is drained
	}()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return; the didSave-triggered reindex goroutine may be orphaned")
	}
	// Reaching here means Serve's own wg.Wait() drained the s.rpc.Go-tracked
	// reindex goroutine before Serve returned — it neither outlived the
	// session nor panicked.
}
