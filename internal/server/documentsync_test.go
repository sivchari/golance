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

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// reindexWait bounds how long tests wait for a didSave-triggered reindex
// to finish. The reindex re-type-checks the saved package, which under
// -race on shared CI runners has taken longer than the 5s this used to
// be; the waits exit as soon as the reindex lands, so a generous bound
// costs nothing when the run is fast.
const reindexWait = 30 * time.Second

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

// writeModuleFile writes content to rel under dir, creating parent
// directories as needed, and returns its absolute path.
func writeModuleFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestHandleDidOpen_QueuedBeforeWorkspaceReadyThenDrained verifies that a
// didOpen arriving while s.workspace() is still nil — the async window
// between handleInitialize returning and its background graph load
// finishing (see lifecycle.go's own doc) — is queued (markPendingOpen)
// rather than silently dropped, and that the very next setWorkspace call
// (loadWorkspaceAsync's own first one, in production) drains it
// (drainPendingOpens), mirroring TestHandleDidSave_
// ReindexedOnceIndexBecomesAvailable's identical markDirty/drainDirty
// proof for a save landing while s.idx is nil.
func TestHandleDidOpen_QueuedBeforeWorkspaceReadyThenDrained(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	file := filepath.Join(root, "greet", "greet.go")
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})

	if ws := s.workspace(); ws != nil {
		t.Fatal("workspace already populated before setWorkspace ever ran; test setup is wrong")
	}

	openParams := mustMarshal(t, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri.File(file), Version: 1, Text: string(text)},
	})
	if err := s.handleDidOpen(context.Background(), openParams); err != nil {
		t.Fatalf("handleDidOpen (workspace not ready): %v", err)
	}

	s.pendingOpensMu.Lock()
	pending := s.pendingOpens[file]
	s.pendingOpensMu.Unlock()
	if !pending {
		t.Fatal("didOpen while workspace was nil did not record a pending open (markPendingOpen); the save would otherwise be lost")
	}

	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	s.setWorkspace(root, snap)

	s.pendingOpensMu.Lock()
	stillPending := s.pendingOpens[file]
	s.pendingOpensMu.Unlock()
	if stillPending {
		t.Fatal("pending open for the file was not drained by setWorkspace")
	}
}

// TestHandleDidSave_TestFileReindexesNewSymbol is a regression test for
// pkgPathForFile's directory fallback (see server.go): before it existed,
// saving an in-package _test.go file resolved no package at all — ws.
// fileToPkg is built from graph.Package.GoFiles alone, which never
// includes test files (see internal/graph's loadMode) — so handleDidSave
// silently returned without ever reindexing it. It saves greet_test.go
// with a newly added, test-only exported symbol and polls workspace/symbol
// until that symbol is findable, verifying the save's reindex actually
// fired and reprocessed the test file. Uses its own synthetic module
// rather than testdata/module, so adding a _test.go file here does not
// perturb any other test's package/symbol-count assumptions against that
// shared fixture.
func TestHandleDidSave_TestFileReindexesNewSymbol(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "go.mod", "module example.com/didsavetest\n\ngo 1.23\n")
	writeModuleFile(t, dir, "greet/greet.go", "package greet\n\n// Hello returns a greeting.\nfunc Hello() string { return \"hi\" }\n")
	const testSrc = "package greet\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n\tif Hello() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n"
	testFile := writeModuleFile(t, dir, "greet/greet_test.go", testSrc)

	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
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
	if _, err := index.Build(context.Background(), snap, db, cas, &index.Options{}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(dir, snap)
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, false)})

	openDoc(t, s, testFile, testSrc)

	const newSymbol = "TestOnlyHelperXYZ"
	edited := testSrc + "\n// " + newSymbol + " is declared only in this in-package test file.\nfunc " + newSymbol + "() int { return 1 }\n"

	saveParams := mustMarshal(t, &protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(testFile)},
		Text:         &edited,
	})
	if err := s.handleDidSave(context.Background(), saveParams); err != nil {
		t.Fatalf("handleDidSave: %v", err)
	}

	deadline := time.Now().Add(reindexWait)
	for {
		resp, err := s.handleWorkspaceSymbol(context.Background(), mustMarshal(t, &protocol.WorkspaceSymbolParams{Query: newSymbol}))
		if err != nil {
			t.Fatalf("handleWorkspaceSymbol: %v", err)
		}
		if syms, ok := resp.(protocol.SymbolInformationSlice); ok && len(syms) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become visible via workspace/symbol after saving the in-package test file; the didSave-triggered reindex may not have fired for it", newSymbol)
		}
		time.Sleep(10 * time.Millisecond)
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
	case <-time.After(reindexWait):
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

	deadline := time.Now().Add(reindexWait)
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
