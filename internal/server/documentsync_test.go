package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// reindexWait bounds TestHandleDidSave_ReindexNeverOrphanedByShutdown's
// deadman select on Serve returning. The reindex re-type-checks the saved
// package, which under -race on shared CI runners has taken longer than the
// 5s this used to be; the select exits as soon as Serve returns, so a
// generous bound costs nothing when the run is fast.
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
	stopWorkspaceEngineOnCleanup(t, s)

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
	synctest.Test(t, func(t *testing.T) {
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
		stopWorkspaceEngineOnCleanup(t, s)
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

		// Blocks until the s.rpc.Go-launched background reindex goroutine
		// exits (real type-checking runs at real speed inside the bubble).
		synctest.Wait()
		resp, err := s.handleWorkspaceSymbol(context.Background(), mustMarshal(t, &protocol.WorkspaceSymbolParams{Query: newSymbol}))
		if err != nil {
			t.Fatalf("handleWorkspaceSymbol: %v", err)
		}
		if syms, ok := resp.(protocol.SymbolInformationSlice); !ok || len(syms) == 0 {
			t.Fatalf("%s not visible via workspace/symbol after saving the in-package test file; the didSave-triggered reindex may not have fired for it", newSymbol)
		}
	})
}

// TestReindex_NarrowsDepCacheInvalidationToActuallyChangedHops verifies
// Server.reindex evicts ws.depCache only for the hops index.Reindex reports
// via Stats.Changed, not the whole reverse-dependency closure: mid is
// imported by top, and a body-only edit to mid must leave top's decoded
// dependency entry alone, while a signature-changing edit must evict it
// too. Presence in depCache is observed indirectly through
// typecheck.Cache.Decodes(): re-importing a path that is still cached is a
// hit (no new decode), while re-importing an evicted path forces a fresh
// one.
func TestReindex_NarrowsDepCacheInvalidationToActuallyChangedHops(t *testing.T) {
	const (
		pkgMid = "example.com/depcachetest/mid"
		pkgTop = "example.com/depcachetest/top"
	)

	tests := []struct {
		name        string
		edited      string
		wantEvicted map[string]bool
	}{
		{
			name: "body only edit",
			edited: `package mid

import "example.com/depcachetest/leaf"

// Shout returns a greeting for name.
func Shout(name string) string {
	return leaf.Hello(name) + "!"
}
`,
			wantEvicted: map[string]bool{pkgMid: true, pkgTop: false},
		},
		{
			name: "signature changing edit",
			edited: `package mid

import "example.com/depcachetest/leaf"

// Shout returns a greeting for name, repeated n times.
func Shout(name string, n int) string {
	out := leaf.Hello(name)
	for i := 1; i < n; i++ {
		out += out
	}
	return out
}
`,
			wantEvicted: map[string]bool{pkgMid: true, pkgTop: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, idx, midFile, _ := newDepCacheReindexServer(t)
			ws := s.workspace()
			// Warm depCache with a decoded entry for both mid and top,
			// mirroring what a real recheck of some other package importing
			// top (and so transitively mid) would leave behind.
			imp := ws.depCache.importer()
			for _, p := range []string{pkgMid, pkgTop} {
				if _, err := imp.ImportFrom(p, "", 0); err != nil {
					t.Fatalf("warm depCache for %s: %v", p, err)
				}
			}

			openDoc(t, s, midFile, tt.edited)
			_ = s.reindex(context.Background(), ws, idx, pkgMid)

			for _, p := range []string{pkgMid, pkgTop} {
				before := ws.depCache.cache.Decodes()
				if _, err := imp.ImportFrom(p, "", 0); err != nil {
					t.Fatalf("re-import %s after reindex: %v", p, err)
				}
				evicted := ws.depCache.cache.Decodes() > before
				if evicted != tt.wantEvicted[p] {
					t.Errorf("%s evicted from depCache = %v, want %v", p, evicted, tt.wantEvicted[p])
				}
			}
		})
	}
}

// newDepCacheReindexServer builds the leaf/mid/top synthetic module,
// indexes it, and returns a workspace-ready server over it plus its
// installed index state and mid's and top's file paths — the fixture
// TestReindex_NarrowsDepCacheInvalidationToActuallyChangedHops and
// TestReindex_PropagatesDependencyAPIChangeToOpenDependent drive.
func newDepCacheReindexServer(t *testing.T) (s *Server, idx *indexState, midFile, topFile string) {
	t.Helper()
	dir := t.TempDir()
	writeModuleFile(t, dir, "go.mod", "module example.com/depcachetest\n\ngo 1.23\n")
	writeModuleFile(t, dir, "leaf/leaf.go", "package leaf\n\n// Hello returns a greeting for name.\nfunc Hello(name string) string { return \"hello \" + name }\n")
	midFile = writeModuleFile(t, dir, "mid/mid.go", `package mid

import "example.com/depcachetest/leaf"

// Shout returns a greeting for name.
func Shout(name string) string {
	return leaf.Hello(name)
}
`)
	topFile = writeModuleFile(t, dir, "top/top.go", `package top

import "example.com/depcachetest/mid"

// Run calls mid.Shout.
func Run(name string) string {
	return mid.Shout(name)
}
`)

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
	s = New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(dir, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	idx = &indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, false)}
	s.idx.Store(idx)
	return s, idx, midFile, topFile
}

// TestReindex_PropagatesDependencyAPIChangeToOpenDependent is a regression
// test for the C2 finding in audit-silent-failures.md: a dependency
// package's exported API changing must reach ws.engine's own per-unit
// cache, not just ws.depCache/idx.resolver — otherwise an already-checked,
// open dependent file keeps serving type information built against the
// dependency's OLD export data indefinitely, since its own content (and so
// its own contentHash) never changed. top imports mid; top is checked (and
// so cached) via the engine first, then mid's exported Shout signature
// changes (an added parameter) and is reindexed the same way a save would.
// Before ws.engine.InvalidateDependency existed, top's next Get returned the
// same stale, error-free CheckedPackage verbatim; with it, the cache entry
// for top's directory is dropped, so the next Get re-type-checks top against
// mid's new signature and surfaces the resulting arity mismatch.
func TestReindex_PropagatesDependencyAPIChangeToOpenDependent(t *testing.T) {
	const pkgMid = "example.com/depcachetest/mid"

	s, idx, midFile, topFile := newDepCacheReindexServer(t)
	ws := s.workspace()

	topSrc, err := os.ReadFile(topFile)
	if err != nil {
		t.Fatalf("read %s: %v", topFile, err)
	}
	openDoc(t, s, topFile, string(topSrc))

	before, err := ws.engine.Get(context.Background(), topFile)
	if err != nil {
		t.Fatalf("Get(top) before mid's signature change: %v", err)
	}
	if diags := check.Diagnostics(before, s.overlay); len(diags) != 0 {
		t.Fatalf("Get(top) before mid's signature change already has diagnostics: %v", diags)
	}

	const editedMid = `package mid

import "example.com/depcachetest/leaf"

// Shout returns a greeting for name, repeated n times.
func Shout(name string, n int) string {
	out := leaf.Hello(name)
	for i := 1; i < n; i++ {
		out += out
	}
	return out
}
`
	// depcheck.Provider (the fallback depexport's ExportData uses for a
	// workspace pkgPath, see internal/depcheck.Provider.Delete's doc) parses
	// straight from disk, never through the overlay, so mid's saved content
	// must actually land on disk for its re-check to see the new signature —
	// mirroring a real save, which writes disk before/alongside didSave.
	if err := os.WriteFile(midFile, []byte(editedMid), 0o600); err != nil {
		t.Fatalf("write %s: %v", midFile, err)
	}
	openDoc(t, s, midFile, editedMid)
	_ = s.reindex(context.Background(), ws, idx, pkgMid)

	after, err := ws.engine.Get(context.Background(), topFile)
	if err != nil {
		t.Fatalf("Get(top) after mid's signature change: %v", err)
	}
	if after == before {
		t.Fatal("Get(top) after mid's signature change returned the same cached CheckedPackage; ws.engine was never invalidated for top's directory")
	}
	diags := check.Diagnostics(after, s.overlay)
	if len(diags) == 0 {
		t.Fatal("Get(top) after mid's signature change still reports no diagnostics; top was rechecked against mid's OLD (cached) signature instead of the new one")
	}
}

// TestReindex_DoesNotInvalidateUnrelatedDependentOnBodyOnlyChange guards the
// cost side of the same fix: reindexing mid with a body-only edit (its
// exported Shout signature unchanged) must not touch top's cached
// CheckedPackage in ws.engine at all, since Stats.Changed — and so the
// reverse-dependency closure ws.engine.InvalidateDependency is scoped
// to — is just {mid} in that case. Observed the same way as its sibling
// test: an untouched cache entry makes Get return the identical
// *CheckedPackage instance.
func TestReindex_DoesNotInvalidateUnrelatedDependentOnBodyOnlyChange(t *testing.T) {
	const pkgMid = "example.com/depcachetest/mid"

	s, idx, midFile, topFile := newDepCacheReindexServer(t)
	ws := s.workspace()

	topSrc, err := os.ReadFile(topFile)
	if err != nil {
		t.Fatalf("read %s: %v", topFile, err)
	}
	openDoc(t, s, topFile, string(topSrc))

	before, err := ws.engine.Get(context.Background(), topFile)
	if err != nil {
		t.Fatalf("Get(top) before mid's body-only edit: %v", err)
	}

	const editedMid = `package mid

import "example.com/depcachetest/leaf"

// Shout returns a greeting for name.
func Shout(name string) string {
	return leaf.Hello(name) + "!"
}
`
	if err := os.WriteFile(midFile, []byte(editedMid), 0o600); err != nil {
		t.Fatalf("write %s: %v", midFile, err)
	}
	openDoc(t, s, midFile, editedMid)
	_ = s.reindex(context.Background(), ws, idx, pkgMid)

	after, err := ws.engine.Get(context.Background(), topFile)
	if err != nil {
		t.Fatalf("Get(top) after mid's body-only edit: %v", err)
	}
	if after != before {
		t.Fatal("Get(top) after mid's body-only edit returned a different CheckedPackage; a body-only dependency edit must not evict an unrelated dependent from ws.engine's cache")
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
	synctest.Test(t, func(t *testing.T) {
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
	})
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
	synctest.Test(t, func(t *testing.T) {
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

		if s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0) {
			t.Fatal("openIndexAfterBuild locked = true, want false")
		}
		idx := s.idx.Load()
		if idx == nil {
			t.Fatal("s.idx is nil after openIndexAfterBuild")
		}
		t.Cleanup(func() { _ = idx.db.Close() })

		// openIndexAfterBuild's drainDirty call above already reindexed greet
		// synchronously; Wait settles any background work regardless.
		synctest.Wait()
		resp, err := s.handleWorkspaceSymbol(context.Background(), mustMarshal(t, &protocol.WorkspaceSymbolParams{Query: newSymbol}))
		if err != nil {
			t.Fatalf("handleWorkspaceSymbol: %v", err)
		}
		if syms, ok := resp.(protocol.SymbolInformationSlice); !ok || len(syms) == 0 {
			t.Fatalf("%s not visible via workspace/symbol after the index became available; the save made while the index was unavailable appears to have been lost", newSymbol)
		}
	})
}

// TestHandleDidClose_ClearsOwnedDiagnostics is a regression test for
// Finding M11: publishDiagnostics only ever notifies for currently open
// files (see its own doc), so a file closed without any later recheck of
// its package used to keep whatever diagnostics it last had forever in the
// client's Problems panel. It pre-populates s.diagFiles the way a prior
// publishDiagnostics call for the file's package would have, then asserts
// handleDidClose republishes an empty diagnostics list for it.
func TestHandleDidClose_ClearsOwnedDiagnostics(t *testing.T) {
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

	s.diagMu.Lock()
	s.diagFiles["example.com/a"] = map[string]bool{file: true}
	s.diagMu.Unlock()

	closeParams := mustMarshal(t, &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
	})
	if err := s.handleDidClose(context.Background(), closeParams); err != nil {
		t.Fatalf("handleDidClose: %v", err)
	}

	written := out.String()
	if !strings.Contains(written, `"method":"textDocument/publishDiagnostics"`) {
		t.Fatalf("handleDidClose did not publish diagnostics for the closed file: %q", written)
	}
	if !strings.Contains(written, `"diagnostics":[]`) {
		t.Fatalf("handleDidClose's publish did not clear diagnostics (want an empty list): %q", written)
	}

	s.diagMu.Lock()
	stillOwned := s.diagFiles["example.com/a"][file]
	s.diagMu.Unlock()
	if stillOwned {
		t.Fatal("s.diagFiles still credits example.com/a with diagnostics for the closed file")
	}
}

// TestHandleDidClose_NoDiagnosticsIsNoOp verifies that closing a file this
// server never published diagnostics for sends no
// textDocument/publishDiagnostics notification at all — handleDidClose's
// clearing behavior (see TestHandleDidClose_ClearsOwnedDiagnostics) must not
// manufacture a spurious empty-diagnostics publish for every close.
func TestHandleDidClose_NoDiagnosticsIsNoOp(t *testing.T) {
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

	closeParams := mustMarshal(t, &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
	})
	if err := s.handleDidClose(context.Background(), closeParams); err != nil {
		t.Fatalf("handleDidClose: %v", err)
	}

	if written := out.String(); strings.Contains(written, `"method":"textDocument/publishDiagnostics"`) {
		t.Fatalf("handleDidClose published diagnostics for a file that never had any: %q", written)
	}
}

// TestHandleDidOpen_SelfHealsMissingFacts is a regression test for GAP 2's
// didOpen path: opening a file whose package the facts index has never
// recorded a store.UnitPointer for at all (the same state GAP 1's
// stale-snapshot bug leaves behind, or simply a package that was never
// saved through the editor after a git checkout/pull) triggers a
// background reindex without requiring a save first.
func TestHandleDidOpen_SelfHealsMissingFacts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		writeModuleFile(t, dir, "go.mod", "module example.com/didopenheal\n\ngo 1.23\n")
		writeModuleFile(t, dir, "pkga/pkga.go", "package pkga\n\n// V returns 1.\nfunc V() int { return 1 }\n")

		snapBeforeB, err := graph.Load(graph.Options{Dir: dir}, "./...")
		if err != nil {
			t.Fatalf("graph.Load: %v", err)
		}

		dbPath := filepath.Join(t.TempDir(), "index.db")
		cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
		if err != nil {
			t.Fatalf("store.OpenCAS: %v", err)
		}
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		if _, err := index.Build(context.Background(), snapBeforeB, db, cas, &index.Options{}); err != nil {
			t.Fatalf("index.Build: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}

		const pkgbSrc = "package pkgb\n\n// W returns 2.\nfunc W() int { return 2 }\n"
		pkgbFile := writeModuleFile(t, dir, "pkgb/pkgb.go", pkgbSrc)
		fullSnap, err := graph.Load(graph.Options{Dir: dir}, "./...")
		if err != nil {
			t.Fatalf("graph.Load: %v", err)
		}

		db2, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open (reopen): %v", err)
		}
		t.Cleanup(func() { _ = db2.Close() })

		rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
		s := New(rpcServer, Options{Logger: newTestLogger(t)})
		s.setWorkspace(dir, fullSnap)
		stopWorkspaceEngineOnCleanup(t, s)
		s.idx.Store(&indexState{db: db2, cas: cas, resolver: xref.New(db2, cas, fullSnap, false)})

		openParams := mustMarshal(t, &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: uri.File(pkgbFile), Version: 1, Text: pkgbSrc},
		})
		if err := s.handleDidOpen(context.Background(), openParams); err != nil {
			t.Fatalf("handleDidOpen: %v", err)
		}
		synctest.Wait()

		if _, err := db2.GetUnit(context.Background(), store.Hash("example.com/didopenheal/pkgb")); err != nil {
			t.Fatalf("GetUnit(pkgb) after didOpen: %v (want its facts self-healed without a save)", err)
		}
	})
}

// TestHandleDidOpen_NoSelfHealWhenIndexNil verifies that selfHealFactsIfStale
// is a plain no-op — no panic, nothing scheduled — while s.idx is nil (the
// same window handleDidSave's own markDirty/drainDirty exists for): the very
// first successful build writes every package's facts from scratch, so
// there is nothing to repair yet, and there is no *indexState to reindex
// through even if there were.
func TestHandleDidOpen_NoSelfHealWhenIndexNil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, snap := newTestServerNoIndex(t)
		file := snap.Packages["example.com/servermod/greet"].GoFiles[0]
		text, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		openParams := mustMarshal(t, &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: uri.File(file), Version: 1, Text: string(text)},
		})
		if err := s.handleDidOpen(context.Background(), openParams); err != nil {
			t.Fatalf("handleDidOpen (index nil): %v", err)
		}
		synctest.Wait()

		if idx := s.idx.Load(); idx != nil {
			t.Fatal("s.idx installed during a test that never built one before the open; selfHealFactsIfStale must not install anything")
		}
	})
}

// TestHandleDidOpen_NoSelfHealWhenFresh verifies that opening a file whose
// package facts are already up to date leaves its store.UnitPointer
// byte-for-byte unchanged: selfHealFactsIfStale must not trigger a reindex
// (and so not rewrite anything) for a package index.PackageChanged reports
// unchanged. Uses its own temp module — not testdata/module, which lives
// inside this repository's own git checkout and so would make
// RelativeIndexPaths(root) true, while newTestServer's index.Build call
// always stores absolute paths (Options{} default); a temp dir outside any
// git repository keeps both consistent with each other, matching what
// index.Build and selfHealFactsIfStale would each independently resolve
// RelativeIndexPaths(root) to in production.
func TestHandleDidOpen_NoSelfHealWhenFresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		writeModuleFile(t, dir, "go.mod", "module example.com/didopenfresh\n\ngo 1.23\n")
		const greetSrc = "package greet\n\n// Hello returns a greeting.\nfunc Hello() string { return \"hi\" }\n"
		greetFile := writeModuleFile(t, dir, "greet/greet.go", greetSrc)

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
		stopWorkspaceEngineOnCleanup(t, s)
		s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, false)})

		const pkgGreet = "example.com/didopenfresh/greet"
		before, err := db.GetUnit(context.Background(), store.Hash(pkgGreet))
		if err != nil {
			t.Fatalf("GetUnit(greet) before didOpen: %v", err)
		}

		openParams := mustMarshal(t, &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: uri.File(greetFile), Version: 1, Text: greetSrc},
		})
		if err := s.handleDidOpen(context.Background(), openParams); err != nil {
			t.Fatalf("handleDidOpen: %v", err)
		}
		synctest.Wait()

		after, err := db.GetUnit(context.Background(), store.Hash(pkgGreet))
		if err != nil {
			t.Fatalf("GetUnit(greet) after didOpen: %v", err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("UnitPointer changed after opening an unmodified file: before=%+v after=%+v (selfHealFactsIfStale must not reindex an up-to-date package)", before, after)
		}
	})
}
