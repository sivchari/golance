package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
)

// newWorkspaceOnlyServer builds a Server with its workspace populated (so
// s.workspace() is non-nil, as it is by the time handleInitialize calls
// tryWarmOpen/buildIndex) but with no facts index open yet.
func newWorkspaceOnlyServer(t *testing.T) *Server {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	return s
}

// openTestCAS returns a fresh CAS under a temp directory.
func openTestCAS(t *testing.T) *store.CAS {
	t.Helper()
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	return cas
}

// buildTestIndexDB runs a full index.Build for snap into a fresh database
// at dbPath and cas (recording its build fingerprint on success) and
// closes the database.
func buildTestIndexDB(t *testing.T, snap *graph.Snapshot, dbPath string, cas *store.CAS) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := index.Build(context.Background(), snap, db, cas, index.Options{RelativePaths: RelativeIndexPaths(snap.Dir())}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
}

// TestOpenIndexAfterBuild_FallsBackToExistingDBOnFailure verifies that an
// indexer failure (waitErr != nil) does not leave the facts index
// unavailable when a database from an earlier successful build already
// exists on disk: it is opened anyway, stale or incomplete being strictly
// better than unavailable.
func TestOpenIndexAfterBuild_FallsBackToExistingDBOnFailure(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	snap := s.workspace().snap
	dbPath := filepath.Join(t.TempDir(), "index.db")
	buildTestIndexDB(t, snap, dbPath, openTestCAS(t))

	s.openIndexAfterBuild(dbPath, errors.New("boom"), "indexer stderr")

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil; want the existing database opened despite the indexer's failure")
	}
	if err := idx.db.Close(); err != nil {
		t.Errorf("db.Close: %v", err)
	}
}

// TestOpenIndexAfterBuild_NoDatabaseStaysUnavailable verifies that an
// indexer failure with no database ever built for this root (the common
// case: a failed graph load or database open, before any package was
// indexed) leaves the facts index unavailable rather than opening (and
// thereby creating) an empty database.
func TestOpenIndexAfterBuild_NoDatabaseStaysUnavailable(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	dbPath := filepath.Join(t.TempDir(), "never-built.db")

	s.openIndexAfterBuild(dbPath, errors.New("boom"), "indexer stderr")

	if idx := s.idx.Load(); idx != nil {
		t.Fatal("idx is non-nil; want nil when the indexer failed and no database was ever built")
	}
}

// TestOpenIndexAfterBuild_Success verifies the ordinary path: no error,
// database opens, idx is installed.
func TestOpenIndexAfterBuild_Success(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	snap := s.workspace().snap
	dbPath := filepath.Join(t.TempDir(), "index.db")
	buildTestIndexDB(t, snap, dbPath, openTestCAS(t))

	s.openIndexAfterBuild(dbPath, nil, "")

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil after a successful build")
	}
	if err := idx.db.Close(); err != nil {
		t.Errorf("db.Close: %v", err)
	}
}

// TestTryWarmOpen_MatchingFingerprintOpensDirectly verifies that a
// database already built with the running toolchain is opened directly,
// without the caller needing to launch the indexer subprocess.
func TestTryWarmOpen_MatchingFingerprintOpensDirectly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	snap := s.workspace().snap

	if idx, ok := s.tryWarmOpen(root); ok || idx != nil {
		t.Fatalf("tryWarmOpen() before any build = (%v, %v), want (nil, false)", idx, ok)
	}

	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, dbPath, cas)

	idx, ok := s.tryWarmOpen(root)
	if !ok || idx == nil {
		t.Fatal("tryWarmOpen() after a matching-fingerprint build = not ok, want ok")
	}
	if err := idx.db.Close(); err != nil {
		t.Errorf("db.Close: %v", err)
	}
}

// TestTryWarmOpen_OpensRegardlessOfFingerprint verifies that a database
// recorded under a different toolchain fingerprint is still opened
// directly: tryWarmOpen no longer gates on the fingerprint (revalidateIndex
// is what catches this and triggers a rebuild, see indexer_test.go's
// TestRevalidateIndex_* below).
func TestTryWarmOpen_OpensRegardlessOfFingerprint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root

	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := db.PutBuildFingerprint("not-the-running-toolchain"); err != nil {
		t.Fatalf("PutBuildFingerprint: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	idx, ok := s.tryWarmOpen(root)
	if !ok || idx == nil {
		t.Fatal("tryWarmOpen() with a mismatched fingerprint = not ok, want ok")
	}
	if err := idx.db.Close(); err != nil {
		t.Errorf("db.Close: %v", err)
	}
}

// TestRevalidateIndex_UnchangedKeepsWarmOpenHandle verifies that when
// nothing has changed since the database was built, revalidateIndex leaves
// the warm-opened *indexState installed (same pointer identity — no
// close-and-rebuild churn) rather than launching a rebuild.
func TestRevalidateIndex_UnchangedKeepsWarmOpenHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	snap := s.workspace().snap

	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, dbPath, cas)

	idx, ok := s.tryWarmOpen(root)
	if !ok {
		t.Fatal("tryWarmOpen() = not ok, want ok")
	}
	s.idx.Store(idx)
	t.Cleanup(func() { _ = idx.db.Close() })

	s.revalidateIndex(context.Background(), root)

	got := s.idx.Load()
	if got != idx {
		t.Errorf("s.idx after revalidateIndex = %p, want the original warm-opened %p (unchanged should not rebuild)", got, idx)
	}
}

// TestRevalidateIndex_SerializedAgainstConcurrentCaller is a race test (run
// with -race) for Finding 2: the post-initialize background check
// (lifecycle.go) and a watched-files-triggered revalidateWorkspace pass
// (workspace.go) both call revalidateIndex, with no synchronization between
// their two goroutines other than s.idxMu. This drives that directly: while
// one caller holds idxMu (simulating an in-flight rebuild), a concurrent
// second call must block instead of running its own body concurrently —
// the property that rules out both a nil-pointer panic racing s.idx.Store
// against idx.db.Close and a second, redundant indexer subprocess. Once
// the lock is released, the second call proceeds and completes, so
// whichever call runs last necessarily re-evaluates and installs against
// the then-current state — "newest build wins" by construction of running
// strictly one at a time, rather than by racing two builds to completion.
func TestRevalidateIndex_SerializedAgainstConcurrentCaller(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root

	s.idxMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		// s.idx is nil, so indexNeedsRebuild is false and this returns
		// immediately once it acquires idxMu — exactly what proves it
		// really was blocked on the lock rather than doing real work.
		s.revalidateIndex(context.Background(), root)
		close(done)
	}()

	<-started
	select {
	case <-done:
		t.Fatal("revalidateIndex returned while s.idxMu was still held by a concurrent caller")
	case <-time.After(100 * time.Millisecond):
	}

	s.idxMu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("revalidateIndex never completed after s.idxMu was released")
	}
}

// TestSpawnIndexer_BoundToContext verifies that spawnIndexer builds its
// *exec.Cmd via exec.CommandContext (Cmd.Cancel is non-nil only when built
// that way), not plain exec.Command — the wiring Finding 6's fix relies on
// so canceling the server's own session-lifetime context (see
// rpc.Server.Context) terminates an in-flight indexer subprocess instead of
// orphaning it on shutdown.
func TestSpawnIndexer_BoundToContext(t *testing.T) {
	cmd := spawnIndexer(context.Background(), "golance-indexer-test-placeholder")
	if cmd.Cancel == nil {
		t.Fatal("spawnIndexer's *exec.Cmd has no Cancel func; want one built via exec.CommandContext so context cancellation terminates the subprocess")
	}
}

// TestIndexStatsMessage verifies indexStatsMessage's parsing of the
// indexer subprocess's final "STATS ..." stdout line (see cmd/golance's
// indexer entry point), including that it rejects anything else
// relayIndexProgress might read off the same stream.
func TestIndexStatsMessage(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantMsg string
		wantOK  bool
	}{
		{
			name:    "typical build",
			line:    "STATS processed=3 skipped=40 errors=0 typechecked=1",
			wantMsg: "1 type-checked, 2 resolved from cache, 40 unchanged, 0 error(s)",
			wantOK:  true,
		},
		{
			name:    "CAS-hit-only build",
			line:    "STATS processed=1 skipped=2 errors=0 typechecked=0",
			wantMsg: "0 type-checked, 1 resolved from cache, 2 unchanged, 0 error(s)",
			wantOK:  true,
		},
		{
			name:   "progress line",
			line:   "PROGRESS 2 3",
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, ok := indexStatsMessage(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("indexStatsMessage(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && msg != tt.wantMsg {
				t.Errorf("indexStatsMessage(%q) = %q, want %q", tt.line, msg, tt.wantMsg)
			}
		})
	}
}

// TestIndexNeedsRebuild_MismatchedFingerprint verifies that a stale
// database (here, a mismatched toolchain fingerprint, which forces every
// package to be treated as changed) is flagged for a rebuild by
// indexNeedsRebuild — the check revalidateIndex uses before closing the
// warm-opened handle and delegating to buildIndex (not exercised directly
// here: buildIndex launches a real subprocess, which is out of scope for a
// unit test — see the e2e suite for full-process coverage).
func TestIndexNeedsRebuild_MismatchedFingerprint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	snap := s.workspace().snap

	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, dbPath, cas)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := db.PutBuildFingerprint("not-the-running-toolchain"); err != nil {
		t.Fatalf("PutBuildFingerprint: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	idx, ok := s.tryWarmOpen(root)
	if !ok {
		t.Fatal("tryWarmOpen() = not ok, want ok")
	}
	s.idx.Store(idx)
	t.Cleanup(func() { _ = idx.db.Close() })

	if !s.indexNeedsRebuild() {
		t.Error("indexNeedsRebuild() = false, want true for a mismatched toolchain fingerprint")
	}
}
