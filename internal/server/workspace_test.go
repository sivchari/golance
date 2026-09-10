package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestWorkspaceReadyRefreshes covers workspaceReadyRefreshes' gating: no
// refresh call is ever returned before s.clientInitialized is set (the LSP
// spec forbids a server-initiated request before the client's "initialized"
// notification, and setWorkspace's first call happens synchronously inside
// handleInitialize, well before that arrives — see refreshOnWorkspaceReady's
// doc), and afterward exactly the capabilities the client declared are
// represented, one refresh call per capability.
func TestWorkspaceReadyRefreshes(t *testing.T) {
	tests := []struct {
		name               string
		clientInitialized  bool
		inlayHintSupport   bool
		semanticTokenSup   bool
		wantRefreshesCount int
	}{
		{"before initialized, no capabilities", false, false, false, 0},
		{"before initialized, both capabilities", false, true, true, 0},
		{"after initialized, no capabilities", true, false, false, 0},
		{"after initialized, inlay hints only", true, true, false, 1},
		{"after initialized, semantic tokens only", true, false, true, 1},
		{"after initialized, both capabilities", true, true, true, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(rpc.NewServer(), Options{Logger: newTestLogger(t)})
			s.clientInitialized.Store(tt.clientInitialized)
			s.inlayHintRefreshSupport.Store(tt.inlayHintSupport)
			s.semanticTokensRefreshSupport.Store(tt.semanticTokenSup)

			got := s.workspaceReadyRefreshes()
			if len(got) != tt.wantRefreshesCount {
				t.Errorf("workspaceReadyRefreshes() returned %d refreshes, want %d", len(got), tt.wantRefreshesCount)
			}
		})
	}
}

// TestRevalidateGraph_RepairsIndexAfterSnapshotSelfHeals is a regression
// test for GAP 1: a warm-opened facts index missing a package's
// UnitPointer entirely, installed alongside a snapshot that did not even
// list that package yet (the state a wrong shared graph-cache snapshot —
// or, here, simply an older on-disk state — leaves behind), must have its
// facts repaired once revalidateGraph installs the real, complete
// snapshot — without needing a save or any other trigger. Before
// revalidateGraph called revalidateIndex itself, this package's facts
// would never be checked at all: the only revalidateIndex passes wired up
// (lifecycle.go's once-after-initialize check, revalidateWorkspace's
// non-reload path) both ran against whatever snapshot was already
// installed, never against the fresher one a reload itself just loaded.
func TestRevalidateGraph_RepairsIndexAfterSnapshotSelfHeals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeTempFile(t, dir, "go.mod", "module example.com/selfheal\n\ngo 1.26\n")
	aDir := filepath.Join(dir, "pkga")
	if err := os.MkdirAll(aDir, 0o750); err != nil {
		t.Fatalf("mkdir pkga: %v", err)
	}
	writeTempFile(t, aDir, "pkga.go", "package pkga\n\n// V returns 1.\nfunc V() int { return 1 }\n")

	// The "wrong" snapshot: loaded before pkgb ever existed on disk, so it
	// does not list it at all — mirroring a shared graph cache reflecting a
	// different worktree's checked-out branch (see graph.Shared's own doc).
	wrongSnap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (wrong snapshot, before pkgb exists): %v", err)
	}
	if _, ok := wrongSnap.Packages["example.com/selfheal/pkgb"]; ok {
		t.Fatal("pkgb already present in the deliberately-incomplete snapshot; test setup is wrong")
	}

	dbPath := indexDBFile(dir)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(dir))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	// The warm-opened database matches the wrong snapshot exactly (as a
	// prior session's own successful build against it would have): nothing
	// looks stale from wrongSnap's own point of view.
	buildTestIndexDB(t, wrongSnap, dbPath, cas)

	bDir := filepath.Join(dir, "pkgb")
	if err := os.MkdirAll(bDir, 0o750); err != nil {
		t.Fatalf("mkdir pkgb: %v", err)
	}
	writeTempFile(t, bDir, "pkgb.go", "package pkgb\n\n// W returns 2.\nfunc W() int { return 2 }\n")

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(dir, wrongSnap)
	stopWorkspaceEngineOnCleanup(t, s)

	idx, ok := s.tryWarmOpen(dir)
	if !ok {
		t.Fatal("tryWarmOpen() = not ok, want ok")
	}
	s.idx.Store(idx)
	t.Cleanup(func() { _ = idx.db.Close() })

	// The once-after-initialize check, run against the wrong snapshot: it
	// cannot find anything wrong with pkgb, since wrongSnap does not even
	// know pkgb exists.
	s.revalidateIndex(context.Background(), dir)
	if _, err := idx.db.GetUnit(context.Background(), store.Hash("example.com/selfheal/pkgb")); err == nil {
		t.Fatal("pkgb already has facts before the reload; test setup no longer isolates what revalidateGraph itself is expected to fix")
	}

	s.revalidateGraph(graph.Options{Dir: dir}, []string{allPackagesPattern})

	if _, ok := s.workspace().snap.Packages["example.com/selfheal/pkgb"]; !ok {
		t.Fatal("revalidateGraph did not install a snapshot listing pkgb")
	}
	// Re-fetch: setWorkspace replaces s.idx's *indexState (a fresh resolver
	// rebuilt over the new snapshot, see its own doc), so the pre-reload idx
	// variable's resolver is now stale, even though it shares the same
	// underlying *store.DB.
	got := s.idx.Load()
	if got == nil {
		t.Fatal("s.idx is nil after revalidateGraph")
	}
	if _, err := got.db.GetUnit(context.Background(), store.Hash("example.com/selfheal/pkgb")); err != nil {
		t.Fatalf("GetUnit(pkgb) after revalidateGraph: %v (want its facts repaired as part of the reload, without any save)", err)
	}
	infos, err := got.resolver.WorkspaceSymbol(context.Background(), "W")
	if err != nil {
		t.Fatalf("WorkspaceSymbol(W): %v", err)
	}
	if len(infos) == 0 {
		t.Fatal(`WorkspaceSymbol("W") returned nothing for pkgb after revalidateGraph, want its facts queryable`)
	}
}

// TestHandleDidChangeWatchedFiles_GraphReloadNeverOrphanedByShutdown is a
// regression test for Finding M6: a go.mod change used to start
// revalidateGraph as a raw goroutine, invisible to Serve's own wg.Wait
// shutdown drain (see Stop's doc), so quitting the editor right after a
// go.mod change could race an in-flight rebuild. Mirrors
// TestHandleDidSave_ReindexNeverOrphanedByShutdown's approach (see
// documentsync_test.go): drive the real wire path so the tracking this
// relies on (s.rpc.Go) is exercised the way production code actually uses
// it — if the reload were still an untracked goroutine, this would still
// pass by luck on a fast machine, but it would no longer be true that
// Serve's return is a reliable signal the reload finished or was canceled.
func TestHandleDidChangeWatchedFiles_GraphReloadNeverOrphanedByShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _, root := newTestServer(t)

		pr, pw := io.Pipe()
		var out bytes.Buffer
		serveDone := make(chan error, 1)
		go func() { serveDone <- s.rpc.Serve(context.Background(), pr, &out) }()

		go func() {
			writeFrame(t, pw, protocol.MethodWorkspaceDidChangeWatchedFiles, &protocol.DidChangeWatchedFilesParams{
				Changes: []protocol.FileEvent{{
					URI:  uri.File(filepath.Join(root, "go.mod")),
					Type: protocol.FileChangeTypeChanged,
				}},
			})
			_ = pw.Close() // EOF: Serve should wind down once the notification (and its tracked graph reload) is drained
		}()

		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("Serve() error = %v", err)
			}
		case <-time.After(reindexWait):
			t.Fatal("Serve() did not return; the go.mod-triggered graph reload goroutine may be orphaned")
		}
		// Reaching here means Serve's own wg.Wait() drained the s.rpc.Go-tracked
		// graph reload before Serve returned.
	})
}
