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

	"github.com/sivchari/golance/internal/store"
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

// TestHandleDidSave_ReindexedOnceIndexBecomesAvailable is a regression test
// for the didSave hole PR #30 left open: handleDidSave silently skipped its
// reindex whenever s.idx was nil (index still building, or briefly swapped
// out mid revalidateIndex) and never retried, permanently losing that save.
//
// It saves greet.go while s.idx is nil (via newTestServerNoIndex), which
// must record the package as dirty (markDirty) instead of reindexing
// immediately; the new content — a brand-new exported symbol — is tracked
// only via the overlay (DidOpen), never written to the on-disk testdata
// fixture. Once an index becomes available — built here from the
// unmodified on-disk content, the same snapshot a build already in flight
// when the save happened would have used — installing it (via
// openIndexAfterBuild, exactly as a real build completion does) must drain
// the dirty set and reindex greet, so the new symbol becomes visible via
// workspace/symbol without any further save.
func TestHandleDidSave_ReindexedOnceIndexBecomesAvailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, snap := newTestServerNoIndex(t)
	root := s.workspace().root

	file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
	original, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	const newSymbol = "GoodbyeDirtySave"
	edited := string(original) + "\n// " + newSymbol + " is added only via the overlay in this test.\nfunc " + newSymbol + "(name string) Greeting {\n\treturn Greeting{Text: \"goodbye, \" + name}\n}\n"

	openDoc(t, s, file, edited)

	saveParams := mustMarshal(t, &protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
	})
	if err := s.handleDidSave(context.Background(), saveParams); err != nil {
		t.Fatalf("handleDidSave (index unavailable): %v", err)
	}
	if idx := s.idx.Load(); idx != nil {
		t.Fatal("s.idx installed during a test that never built one before the save; want nil")
	}
	s.dirtyMu.Lock()
	dirty := s.dirtyPkgs["example.com/servermod/greet"]
	s.dirtyMu.Unlock()
	if !dirty {
		t.Fatal("greet not recorded dirty after a save while the index was unavailable")
	}

	// Build and install an index from the unmodified on-disk content — the
	// snapshot a build already running when the save above happened would
	// have used. newSymbol must be absent from it.
	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, dbPath, cas)

	if s.openIndexAfterBuild(context.Background(), dbPath, nil, "") {
		t.Fatal("openIndexAfterBuild locked = true, want false")
	}
	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("s.idx is nil after openIndexAfterBuild")
	}
	t.Cleanup(func() { _ = idx.db.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := s.handleWorkspaceSymbol(context.Background(), mustMarshal(t, &protocol.WorkspaceSymbolParams{Query: newSymbol}))
		if err != nil {
			t.Fatalf("handleWorkspaceSymbol: %v", err)
		}
		if syms, ok := resp.(protocol.SymbolInformationSlice); ok && len(syms) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become visible via workspace/symbol after the index became available; the save made while the index was unavailable appears to have been lost", newSymbol)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
