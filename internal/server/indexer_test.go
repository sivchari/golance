package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"go.lsp.dev/protocol"
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
	stopWorkspaceEngineOnCleanup(t, s)
	return s
}

// newWorkspaceOnlyServerWithLogBuffer is newWorkspaceOnlyServer with s's own
// logger backed by a buffer instead of t.Logf, so a test can assert on the
// exact text openIndexAfterBuild logs (e.g. the indexer stderr block or the
// errors=N warning) rather than only on s.idx's resulting state.
func newWorkspaceOnlyServerWithLogBuffer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	var buf bytes.Buffer
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: log.New(&buf, "", 0)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	return s, &buf
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
	if _, err := index.Build(context.Background(), snap, db, cas, &index.Options{RelativePaths: RelativeIndexPaths(snap.Dir())}); err != nil {
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

	s.openIndexAfterBuild(context.Background(), dbPath, errors.New("boom"), "indexer stderr", 0)

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

	s.openIndexAfterBuild(context.Background(), dbPath, errors.New("boom"), "indexer stderr", 0)

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

	s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0)

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil after a successful build")
	}
	if err := idx.db.Close(); err != nil {
		t.Errorf("db.Close: %v", err)
	}
}

// TestOpenIndexAfterBuild_CleanExitLogsNonEmptyStderr verifies that a
// clean-exit build's stderr is no longer silently discarded: per
// internal/index.Build's contract, a non-zero exit is reserved for
// conditions that leave the whole build untrustworthy, so a per-package
// panic or unexpected diagnostic that still leaves waitErr nil would
// otherwise never appear in the log at all.
func TestOpenIndexAfterBuild_CleanExitLogsNonEmptyStderr(t *testing.T) {
	s, logBuf := newWorkspaceOnlyServerWithLogBuffer(t)
	snap := s.workspace().snap
	dbPath := filepath.Join(t.TempDir(), "index.db")
	buildTestIndexDB(t, snap, dbPath, openTestCAS(t))

	s.openIndexAfterBuild(context.Background(), dbPath, nil, "panic: something went wrong\n", 0)

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil after a successful build")
	}
	t.Cleanup(func() { _ = idx.db.Close() })

	if !strings.Contains(logBuf.String(), "panic: something went wrong") {
		t.Errorf("log output = %q, want it to contain the non-empty stderr from a clean-exit build", logBuf.String())
	}
}

// TestOpenIndexAfterBuild_NoStderrLogsNothingExtra verifies the converse of
// TestOpenIndexAfterBuild_CleanExitLogsNonEmptyStderr: an empty stderr on a
// clean exit must not log an empty stderr block.
func TestOpenIndexAfterBuild_NoStderrLogsNothingExtra(t *testing.T) {
	s, logBuf := newWorkspaceOnlyServerWithLogBuffer(t)
	snap := s.workspace().snap
	dbPath := filepath.Join(t.TempDir(), "index.db")
	buildTestIndexDB(t, snap, dbPath, openTestCAS(t))

	s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0)

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil after a successful build")
	}
	t.Cleanup(func() { _ = idx.db.Close() })

	if strings.Contains(logBuf.String(), "indexer stderr") {
		t.Errorf("log output = %q, want no stderr block logged when stderr was empty", logBuf.String())
	}
}

// TestOpenIndexAfterBuild_StatsErrorsLogsWarning verifies that statsErrors >
// 0 (the indexer's own "STATS ... errors=N" count — see indexStatsMessage)
// produces a visible warning naming the count, instead of relying solely on
// the $/progress "end" notification's Message, which many clients ignore.
func TestOpenIndexAfterBuild_StatsErrorsLogsWarning(t *testing.T) {
	s, logBuf := newWorkspaceOnlyServerWithLogBuffer(t)
	snap := s.workspace().snap
	dbPath := filepath.Join(t.TempDir(), "index.db")
	buildTestIndexDB(t, snap, dbPath, openTestCAS(t))

	s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 3)

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil after a successful build")
	}
	t.Cleanup(func() { _ = idx.db.Close() })

	got := logBuf.String()
	if !strings.Contains(got, "3") {
		t.Errorf("log output = %q, want it to name the package error count (3)", got)
	}
}

// TestOpenIndexAfterBuild_ZeroStatsErrorsLogsNoWarning verifies the converse
// of TestOpenIndexAfterBuild_StatsErrorsLogsWarning: statsErrors == 0 must
// not produce the warning.
func TestOpenIndexAfterBuild_ZeroStatsErrorsLogsNoWarning(t *testing.T) {
	s, logBuf := newWorkspaceOnlyServerWithLogBuffer(t)
	snap := s.workspace().snap
	dbPath := filepath.Join(t.TempDir(), "index.db")
	buildTestIndexDB(t, snap, dbPath, openTestCAS(t))

	s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0)

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil after a successful build")
	}
	t.Cleanup(func() { _ = idx.db.Close() })

	if strings.Contains(logBuf.String(), "package error(s)") {
		t.Errorf("log output = %q, want no package-error warning when statsErrors is 0", logBuf.String())
	}
}

// TestOpenIndexAfterBuild_SuccessResetsWarnFlags verifies that a successful
// build resets indexBuildingWarned/indexFailedWarned: indexUnavailableError
// and resolverOrWarn's own logMessage key their wording off these flags
// (see indexUnavailableError's doc), so leaving them permanently true from
// an earlier build attempt would keep describing that stale attempt (e.g.
// still claiming "failed to build") the next time s.idx goes nil again,
// long after a later rebuild actually succeeded.
func TestOpenIndexAfterBuild_SuccessResetsWarnFlags(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	snap := s.workspace().snap
	dbPath := filepath.Join(t.TempDir(), "index.db")
	buildTestIndexDB(t, snap, dbPath, openTestCAS(t))

	s.indexBuildingWarned.Store(true)
	s.indexFailedWarned.Store(true)

	s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0)

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("idx is nil after a successful build")
	}
	t.Cleanup(func() {
		if err := idx.db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})

	if s.indexBuildingWarned.Load() {
		t.Error("indexBuildingWarned still true after a successful build, want reset to false")
	}
	if s.indexFailedWarned.Load() {
		t.Error("indexFailedWarned still true after a successful build, want reset to false")
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
		name     string
		line     string
		wantMsg  string
		wantErrs int
		wantOK   bool
	}{
		{
			name:     "typical build",
			line:     "STATS processed=3 skipped=40 errors=0 typechecked=1",
			wantMsg:  "1 type-checked, 2 resolved from cache, 40 unchanged, 0 error(s)",
			wantErrs: 0,
			wantOK:   true,
		},
		{
			name:     "CAS-hit-only build",
			line:     "STATS processed=1 skipped=2 errors=0 typechecked=0",
			wantMsg:  "0 type-checked, 1 resolved from cache, 2 unchanged, 0 error(s)",
			wantErrs: 0,
			wantOK:   true,
		},
		{
			name:     "build with package errors",
			line:     "STATS processed=5 skipped=10 errors=2 typechecked=5",
			wantMsg:  "5 type-checked, 0 resolved from cache, 10 unchanged, 2 error(s)",
			wantErrs: 2,
			wantOK:   true,
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
			msg, errs, ok := indexStatsMessage(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("indexStatsMessage(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && msg != tt.wantMsg {
				t.Errorf("indexStatsMessage(%q) msg = %q, want %q", tt.line, msg, tt.wantMsg)
			}
			if ok && errs != tt.wantErrs {
				t.Errorf("indexStatsMessage(%q) errs = %d, want %d", tt.line, errs, tt.wantErrs)
			}
		})
	}
}

// newNotifyCaptureServer builds a workspace-only Server exactly like
// newWorkspaceOnlyServer, wired over an rpc.Server whose outbound
// notifications land in the returned buffer instead of nowhere — for
// asserting on exactly which $/progress or window/* notifications a call
// sends. Mirrors TestResolverOrWarn_UsesLogMessageNotShowMessage's own
// pattern: Serve is run to completion over an already-closed pipe first, so
// its later Notify calls have a clean happens-before edge into out with
// nothing left concurrent.
func newNotifyCaptureServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

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
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	return s, &out
}

// TestRelayIndexProgress_DoesNotSendEndNotification is a regression test for
// the informational audit's Finding 2 (the hover/completion-doc race): the
// $/progress "end" notification must never come from relayIndexProgress
// itself — only from notifyIndexProgressEnd, called by runIndexBuild once
// openIndexAfterBuild has actually installed s.idx. relayIndexProgress used
// to send "end" the instant the indexer subprocess's stdout stream closed,
// which is always strictly before openIndexAfterBuild even starts (see
// notifyIndexProgressEnd's own doc) — a client that treated that "end" as
// "the index is now queryable" could read s.idx while it was still nil.
func TestRelayIndexProgress_DoesNotSendEndNotification(t *testing.T) {
	s, out := newNotifyCaptureServer(t)

	r := strings.NewReader("PROGRESS 1 2\nPROGRESS 2 2\nSTATS processed=2 skipped=0 errors=0 typechecked=2\n")
	statsErrors, began, summary := s.relayIndexProgress(r)
	if statsErrors != 0 || !began || summary == "" {
		t.Fatalf("relayIndexProgress() = (%d, %v, %q), want (0, true, non-empty)", statsErrors, began, summary)
	}

	written := out.String()
	if !strings.Contains(written, `"kind":"begin"`) {
		t.Errorf("relayIndexProgress() did not send a begin notification: %q", written)
	}
	if !strings.Contains(written, `"kind":"report"`) {
		t.Errorf("relayIndexProgress() did not send a report notification: %q", written)
	}
	if strings.Contains(written, `"kind":"end"`) {
		t.Errorf("relayIndexProgress() sent an end notification itself, want none (only notifyIndexProgressEnd may): %q", written)
	}
}

// TestNotifyIndexProgressEnd_SendsEndWithSummary verifies
// notifyIndexProgressEnd's own half of the contract
// TestRelayIndexProgress_DoesNotSendEndNotification pins the other half of:
// given relayIndexProgress's own (began, summary) result, it sends exactly
// one $/progress "end" carrying summary as its Message, and is a no-op when
// began is false (nothing to end).
func TestNotifyIndexProgressEnd_SendsEndWithSummary(t *testing.T) {
	s, out := newNotifyCaptureServer(t)

	s.notifyIndexProgressEnd(true, "2 type-checked, 0 error(s)")
	written := out.String()
	if !strings.Contains(written, `"kind":"end"`) {
		t.Errorf("notifyIndexProgressEnd(true, ...) did not send an end notification: %q", written)
	}
	if !strings.Contains(written, "2 type-checked, 0 error(s)") {
		t.Errorf("notifyIndexProgressEnd(true, ...) end notification missing its summary Message: %q", written)
	}

	out.Reset()
	s.notifyIndexProgressEnd(false, "should not appear")
	if out.Len() != 0 {
		t.Errorf("notifyIndexProgressEnd(false, ...) sent %q, want nothing (no matching begin)", out.String())
	}
}

// TestRelayIndexProgress_ReturnsStatsErrors verifies relayIndexProgress
// plumbs the "STATS ... errors=N" line's own count back to its caller
// (runIndexBuild, which passes it on to openIndexAfterBuild), not just into
// the $/progress "end" notification's Message.
func TestRelayIndexProgress_ReturnsStatsErrors(t *testing.T) {
	s := newWorkspaceOnlyServer(t)

	r := strings.NewReader("PROGRESS 1 2\nPROGRESS 2 2\nSTATS processed=2 skipped=0 errors=4 typechecked=2\n")
	if got, _, _ := s.relayIndexProgress(r); got != 4 {
		t.Errorf("relayIndexProgress() = %d, want 4", got)
	}
}

// TestRelayIndexProgress_NoStatsLineReturnsZero verifies the converse: no
// "STATS ..." line at all (e.g. the subprocess died before ever writing
// one) must not be mistaken for a package error count.
func TestRelayIndexProgress_NoStatsLineReturnsZero(t *testing.T) {
	s := newWorkspaceOnlyServer(t)

	r := strings.NewReader("PROGRESS 1 2\n")
	if got, _, _ := s.relayIndexProgress(r); got != 0 {
		t.Errorf("relayIndexProgress() = %d, want 0", got)
	}
}

// TestRelayIndexProgress_LineLargerThanOldDefaultBufferStillParses is a
// regression test for Finding L12: relayIndexProgress used to build its
// bufio.Scanner with no explicit buffer size, relying on the package's
// unstated default (bufio.MaxScanTokenSize, 64KiB). A line past that but
// still well within the new explicit maxProgressLine bound (1 MiB) used to
// kill the whole scan — dropping every later line, including the STATS line
// statsErrors depends on — and must not anymore.
func TestRelayIndexProgress_LineLargerThanOldDefaultBufferStillParses(t *testing.T) {
	s := newWorkspaceOnlyServer(t)

	long := strings.Repeat("x", 100*1024) // > 64KiB, < maxProgressLine
	r := strings.NewReader(long + "\nPROGRESS 1 2\nSTATS processed=2 skipped=0 errors=4 typechecked=2\n")
	if got, _, _ := s.relayIndexProgress(r); got != 4 {
		t.Errorf("relayIndexProgress() = %d, want 4 (a line past the old implicit default must not drop the rest of the stream)", got)
	}
}

// TestRelayIndexProgress_OversizedLineIsLoggedByName is a regression test
// for Finding L12: a line past maxProgressLine still ends the scan (a
// bufio.Scanner cannot resume past a too-long token), but must name the
// cause explicitly — not the same generic "read indexer progress" message
// every other read failure gets — since it specifically means the STATS
// line statsErrors depends on was likely never reached.
func TestRelayIndexProgress_OversizedLineIsLoggedByName(t *testing.T) {
	s, logs := newWorkspaceOnlyServerWithLogBuffer(t)

	huge := strings.Repeat("x", 2<<20) // > maxProgressLine (1 MiB)
	r := strings.NewReader("PROGRESS 1 2\n" + huge + "\nSTATS processed=2 skipped=0 errors=4 typechecked=2\n")
	if got, _, _ := s.relayIndexProgress(r); got != 0 {
		t.Errorf("relayIndexProgress() = %d, want 0 (the STATS line after the oversized one must never be reached)", got)
	}
	if !strings.Contains(logs.String(), "progress line exceeded") {
		t.Errorf("log output = %q, want a message naming the oversized-line cause", logs.String())
	}
}

// TestStaleIndexPackages_MismatchedFingerprint verifies that a stale
// database (here, a mismatched toolchain fingerprint) is reported via
// staleIndexPackages' wholeDBStale return — the whole-database short
// circuit revalidateIndex uses to route straight to a full rebuild (not
// exercised directly here: buildIndex launches a real subprocess, which is
// out of scope for a unit test — see the e2e suite for full-process
// coverage) rather than a per-package targeted repair, since Reindex never
// writes a build fingerprint (see index.RevalidateStale's own doc).
func TestStaleIndexPackages_MismatchedFingerprint(t *testing.T) {
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

	pkgs, wholeDBStale := s.staleIndexPackages(context.Background())
	if !wholeDBStale {
		t.Error("staleIndexPackages() wholeDBStale = false, want true for a mismatched toolchain fingerprint")
	}
	if len(pkgs) != 0 {
		t.Errorf("staleIndexPackages() pkgs = %v, want empty when wholeDBStale", pkgs)
	}
}

// TestRevalidateIndex_TargetedRepairFixesMissingPackageWithoutFullRebuild
// verifies GAP 1/D3's repair path end to end: a package that was never
// built into the warm-opened database (its UnitPointer missing entirely —
// the same state a worktree's self-healed snapshot exposes for a package
// its earlier, wrong snapshot never even listed) is fixed in place, and
// s.idx's own pointer identity is left unchanged — proof this went through
// repairIndexPackagesLocked, not a close-and-rebuild.
func TestRevalidateIndex_TargetedRepairFixesMissingPackageWithoutFullRebuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeTempFile(t, dir, "go.mod", "module example.com/repairtest\n\ngo 1.26\n")
	aDir := filepath.Join(dir, "pkga")
	if err := os.MkdirAll(aDir, 0o750); err != nil {
		t.Fatalf("mkdir pkga: %v", err)
	}
	writeTempFile(t, aDir, "pkga.go", "package pkga\n\n// V returns 1.\nfunc V() int { return 1 }\n")

	snapBeforeB, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (before pkgb exists): %v", err)
	}

	dbPath := indexDBFile(dir)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(dir))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	// Builds a database that has never even heard of pkgb — the same state
	// a stale/superseded snapshot (GAP 1) leaves behind, distinct from a
	// package whose content merely changed.
	buildTestIndexDB(t, snapBeforeB, dbPath, cas)

	bDir := filepath.Join(dir, "pkgb")
	if err := os.MkdirAll(bDir, 0o750); err != nil {
		t.Fatalf("mkdir pkgb: %v", err)
	}
	writeTempFile(t, bDir, "pkgb.go", "package pkgb\n\n// W returns 2.\nfunc W() int { return 2 }\n")
	fullSnap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (with pkgb): %v", err)
	}
	if _, ok := fullSnap.Packages["example.com/repairtest/pkgb"]; !ok {
		t.Fatal("pkgb missing from fullSnap; test setup is wrong")
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(dir, fullSnap)
	stopWorkspaceEngineOnCleanup(t, s)

	idx, ok := s.tryWarmOpen(dir)
	if !ok {
		t.Fatal("tryWarmOpen() = not ok, want ok")
	}
	s.idx.Store(idx)
	t.Cleanup(func() { _ = idx.db.Close() })

	s.revalidateIndex(context.Background(), dir)

	got := s.idx.Load()
	if got != idx {
		t.Errorf("s.idx after revalidateIndex = %p, want the original warm-opened %p (a targeted repair must not close-and-rebuild)", got, idx)
	}
	if _, err := got.db.GetUnit(context.Background(), store.Hash("example.com/repairtest/pkgb")); err != nil {
		t.Fatalf("GetUnit(pkgb) after revalidateIndex: %v (want its facts repaired in place)", err)
	}
	infos, err := idx.resolver.WorkspaceSymbol(context.Background(), "W")
	if err != nil {
		t.Fatalf("WorkspaceSymbol(W): %v", err)
	}
	if len(infos) == 0 {
		t.Fatal(`WorkspaceSymbol("W") returned nothing for pkgb after the repair, want its facts queryable exactly like a normally-built package`)
	}
}

// TestRepairIndexPackagesLocked_ReportsFailureViaLogMessage is a regression
// test for Finding H2: revalidateIndex's cold-start self-heal repair pass
// used to leave the only trace of a package it failed to repair in the
// server's own log file (s.reindex's internal s.logger.Printf) -- nothing a
// client, or a user who never opens that log, could ever see. pkgb starts
// out stale exactly like TestRevalidateIndex_TargetedRepairFixesMissingPackageWithoutFullRebuild
// (a UnitPointer missing entirely from the warm-opened database), but its
// own source file is removed from disk before the repair runs, forcing
// index.Reindex to fail for a real reason (a read error, not a type error —
// a type error records a diagnostic and still counts as success), so the
// repair must now report that failure to the client via window/logMessage.
func TestRepairIndexPackagesLocked_ReportsFailureViaLogMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeTempFile(t, dir, "go.mod", "module example.com/repairfailtest\n\ngo 1.26\n")
	aDir := filepath.Join(dir, "pkga")
	if err := os.MkdirAll(aDir, 0o750); err != nil {
		t.Fatalf("mkdir pkga: %v", err)
	}
	writeTempFile(t, aDir, "pkga.go", "package pkga\n\n// V returns 1.\nfunc V() int { return 1 }\n")

	snapBeforeB, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (before pkgb exists): %v", err)
	}

	dbPath := indexDBFile(dir)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(dir))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snapBeforeB, dbPath, cas)

	bDir := filepath.Join(dir, "pkgb")
	if err := os.MkdirAll(bDir, 0o750); err != nil {
		t.Fatalf("mkdir pkgb: %v", err)
	}
	pkgbFile := writeTempFile(t, bDir, "pkgb.go", "package pkgb\n\n// W returns 2.\nfunc W() int { return 2 }\n")
	fullSnap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (with pkgb): %v", err)
	}
	if _, ok := fullSnap.Packages["example.com/repairfailtest/pkgb"]; !ok {
		t.Fatal("pkgb missing from fullSnap; test setup is wrong")
	}

	// pkgb is still known to fullSnap (staleIndexPackages flags it exactly
	// as the successful-repair test above does, from the snapshot/database
	// mismatch alone), but its own file is now gone from disk -- forcing the
	// repair itself to fail for real instead of merely recording a
	// type-check diagnostic.
	if err := os.Remove(pkgbFile); err != nil {
		t.Fatalf("remove pkgb.go: %v", err)
	}

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
	s.setWorkspace(dir, fullSnap)
	stopWorkspaceEngineOnCleanup(t, s)

	idx, ok := s.tryWarmOpen(dir)
	if !ok {
		t.Fatal("tryWarmOpen() = not ok, want ok")
	}
	s.idx.Store(idx)
	t.Cleanup(func() { _ = idx.db.Close() })

	s.revalidateIndex(context.Background(), dir)

	written := out.String()
	if !strings.Contains(written, protocol.MethodWindowLogMessage) {
		t.Errorf("revalidateIndex did not send %s for the failed repair: %q", protocol.MethodWindowLogMessage, written)
	}
	if !strings.Contains(written, "pkgb") {
		t.Errorf("revalidateIndex's failure notice did not name the failed package (pkgb): %q", written)
	}
}

// TestRevalidateIndex_LargeStaleSetFallsBackToFullRebuild verifies
// chooseIndexRevalidateAction's threshold gate: once the stale set exceeds
// indexRepairThreshold, revalidateIndex must choose a full rebuild rather
// than a targeted repair, even though the whole database is not stale.
// Asserted at the decision level (chooseIndexRevalidateAction), not by
// driving revalidateIndex's actual rebuild branch end to end: that
// launches a real indexer subprocess via os.Executable(), which in a `go
// test` binary is the test binary itself — not cmd/golance's indexer mode
// — so letting it run here would at best hang and at worst recursively
// re-execute this very test suite. workspace.go's workspaceReadyRefreshes
// documents the same "test the decision, not the dispatch" split for an
// analogous reason.
func TestRevalidateIndex_LargeStaleSetFallsBackToFullRebuild(t *testing.T) {
	old := indexRepairThreshold
	indexRepairThreshold = 1
	t.Cleanup(func() { indexRepairThreshold = old })

	pkgs := []string{"a", "b"}
	if got := chooseIndexRevalidateAction(pkgs, false); got != indexRevalidateRebuild {
		t.Errorf("chooseIndexRevalidateAction(%d stale, wholeDBStale=false) = %v, want indexRevalidateRebuild once the stale count exceeds indexRepairThreshold=%d", len(pkgs), got, indexRepairThreshold)
	}
	if got := chooseIndexRevalidateAction(pkgs[:1], false); got != indexRevalidateRepair {
		t.Errorf("chooseIndexRevalidateAction(%d stale, wholeDBStale=false) = %v, want indexRevalidateRepair at exactly indexRepairThreshold=%d", 1, got, indexRepairThreshold)
	}
}

// TestOpenIndexAfterBuild_LockedSharedIndexFallsBackToPrivateIndex is a
// regression test for the multi-editor scenario: a second live session
// finding the shared per-root index locked must not simply go without
// cross-reference features for the rest of the session. It simulates a
// first session already holding the shared database's lock, verifies
// openIndexAfterBuild reports locked=true for it (never installing s.idx),
// then drives the same recovery buildIndexLocked performs — switchToPrivateIndex
// followed by a build against the now-private dbPath — entirely in-process
// (via index.Build, not a real indexer subprocess: see buildTestIndexDB),
// and asserts the resulting index is fully usable.
func TestOpenIndexAfterBuild_LockedSharedIndexFallsBackToPrivateIndex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	snap := s.workspace().snap

	sharedDBPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(sharedDBPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	// Simulate a first live session already holding the shared database's
	// exclusive lock (see store.Open's doc).
	held, err := store.Open(sharedDBPath)
	if err != nil {
		t.Fatalf("store.Open (simulated first session): %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	locked := s.openIndexAfterBuild(context.Background(), sharedDBPath, nil, "", 0)
	if !locked {
		t.Fatal("openIndexAfterBuild(shared, locked by another session) locked = false, want true")
	}
	if idx := s.idx.Load(); idx != nil {
		t.Fatal("s.idx installed from a locked open attempt; want nil")
	}

	s.switchToPrivateIndex()
	if !s.usePrivateIndex.Load() {
		t.Fatal("usePrivateIndex not set after switchToPrivateIndex")
	}

	privateDBPath := s.dbPath(root)
	if privateDBPath == sharedDBPath {
		t.Fatalf("dbPath() after switchToPrivateIndex = %s, want a different, private path", privateDBPath)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, privateDBPath, cas)

	locked = s.openIndexAfterBuild(context.Background(), privateDBPath, nil, "", 0)
	if locked {
		t.Fatal("openIndexAfterBuild(private) locked = true, want false")
	}
	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("s.idx is nil after installing the session-private index")
	}
	t.Cleanup(func() { _ = idx.db.Close() })

	infos, err := idx.resolver.WorkspaceSymbol(context.Background(), "Hello")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}
	if len(infos) == 0 {
		t.Fatal(`WorkspaceSymbol("Hello") returned nothing from the session-private index, want it to resolve exactly as the shared index would`)
	}
}

// TestStop_RemovesPrivateIndexFiles verifies that Stop removes a session's
// own private index database (see privateIndexDBFile) once it has fallen
// back to one, so a crashed-free session never leaks these files.
func TestStop_RemovesPrivateIndexFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	snap := s.workspace().snap

	s.switchToPrivateIndex()
	privateDBPath := s.dbPath(root)
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, privateDBPath, cas)

	if s.openIndexAfterBuild(context.Background(), privateDBPath, nil, "", 0) {
		t.Fatal("openIndexAfterBuild(private) locked = true, want false")
	}
	if s.idx.Load() == nil {
		t.Fatal("s.idx is nil after installing the session-private index")
	}
	if _, err := os.Stat(privateDBPath); err != nil {
		t.Fatalf("private index file missing before Stop: %v", err)
	}

	s.Stop()

	if _, err := os.Stat(privateDBPath); !os.IsNotExist(err) {
		t.Fatalf("private index file still exists after Stop: err=%v, want IsNotExist", err)
	}
}

// TestCleanupOrphanedPrivateIndexes verifies that an ownerless private
// index file (its lock immediately acquirable — the signature of a
// crashed session's leftover, see store.TryClaimAbandoned) is removed,
// while the shared index file and a private index file still genuinely
// locked by another live session are both left untouched.
func TestCleanupOrphanedPrivateIndexes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root

	sharedDBPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(sharedDBPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	// The shared file: its name never contains privateIndexInfix, so the
	// cleanup glob must never even consider it, let alone remove it.
	if err := os.WriteFile(sharedDBPath, []byte("shared"), 0o600); err != nil {
		t.Fatalf("write shared decoy file: %v", err)
	}

	// An orphaned private index: created, then its lock released (no
	// process holds it anymore), simulating a crashed prior session.
	orphanPath := privateIndexDBFile(root, "orphan-session")
	orphanDB, err := store.Open(orphanPath)
	if err != nil {
		t.Fatalf("store.Open (orphan): %v", err)
	}
	if err := orphanDB.Close(); err != nil {
		t.Fatalf("close orphan db: %v", err)
	}

	// A private index still genuinely in use by another live session.
	livePath := privateIndexDBFile(root, "live-session")
	liveDB, err := store.Open(livePath)
	if err != nil {
		t.Fatalf("store.Open (live): %v", err)
	}
	t.Cleanup(func() { _ = liveDB.Close() })

	s.cleanupOrphanedPrivateIndexes(root)

	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphaned private index not removed: err=%v, want IsNotExist", err)
	}
	if _, err := os.Stat(sharedDBPath); err != nil {
		t.Errorf("shared index file was touched by cleanup (must never be): %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("locked private index (still in use by a live session) was removed: %v", err)
	}
}
