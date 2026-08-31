package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
)

// TestWatchDebouncerCoalescesBurstIntoOneRun verifies that a burst of
// onEvent calls arriving faster than the debounce delay collapses into a
// single run, carrying reload=true if any event in the burst needed it —
// the behavior a `git pull` touching thousands of files in rapid
// succession relies on.
func TestWatchDebouncerCoalescesBurstIntoOneRun(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	w := newWatchDebouncer(20*time.Millisecond, func(_ string, reload bool) {
		mu.Lock()
		calls = append(calls, reload)
		mu.Unlock()
	})

	for i := range 5 {
		w.onEvent("root", i == 2) // exactly one event in the burst needs reload
		time.Sleep(2 * time.Millisecond)
	}

	waitForCalls(t, &mu, &calls, 1)
	// Give any unwanted extra run a chance to also land before asserting.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("run called %d times, want exactly 1: %v", len(calls), calls)
	}
	if !calls[0] {
		t.Fatal("run reload = false, want true (one event in the burst needed reload)")
	}
}

// TestWatchDebouncerRerunsExactlyOnceAfterInFlightPass verifies the
// singleflight-with-one-pending-rerun scheme: events that arrive while a
// pass is already running are coalesced into exactly one more pass, run
// immediately after the first finishes, and the two passes never overlap.
func TestWatchDebouncerRerunsExactlyOnceAfterInFlightPass(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	var running, maxRunning atomic.Int32

	firstStarted := make(chan struct{})
	release := make(chan struct{})

	w := newWatchDebouncer(5*time.Millisecond, func(_ string, reload bool) {
		n := running.Add(1)
		for {
			old := maxRunning.Load()
			if n <= old || maxRunning.CompareAndSwap(old, n) {
				break
			}
		}

		mu.Lock()
		calls = append(calls, reload)
		first := len(calls) == 1
		mu.Unlock()
		if first {
			close(firstStarted)
			<-release
		}
		running.Add(-1)
	})

	w.onEvent("root", false)
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first run never started")
	}

	// Events arriving while the first pass is running: only their combined
	// reload flag (true, from the last one) should reach the rerun.
	w.onEvent("root", false)
	w.onEvent("root", true)
	close(release)

	waitForCalls(t, &mu, &calls, 2)
	time.Sleep(50 * time.Millisecond) // make sure no unwanted third run sneaks in

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("run called %d times, want exactly 2: %v", len(calls), calls)
	}
	if calls[0] || !calls[1] {
		t.Fatalf("calls = %v, want [false true]", calls)
	}
	if got := maxRunning.Load(); got > 1 {
		t.Fatalf("max concurrent runs = %d, want at most 1 (singleflight)", got)
	}
}

// TestWatchDebouncerStop_PendingTimerNeverFires covers half of Finding 6's
// fix: a debounce timer scheduled but not yet fired must never fire after
// Stop — otherwise a pending workspace/didChangeWatchedFiles-triggered
// revalidation would still run past server shutdown.
func TestWatchDebouncerStop_PendingTimerNeverFires(t *testing.T) {
	var mu sync.Mutex
	var calls int
	w := newWatchDebouncer(20*time.Millisecond, func(_ string, _ bool) {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	w.onEvent("root", false)
	w.Stop()
	time.Sleep(100 * time.Millisecond) // well past the debounce delay, if it were still armed

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("run called %d time(s) after Stop, want 0 (the pending timer must never fire)", got)
	}
}

// TestWatchDebouncerStop_WaitsForInFlightRun covers the other half: Stop
// blocks until a run already in flight when it is called actually finishes,
// rather than returning while it is still outstanding — the property a
// shutdown-time goroutine-leak check relies on.
func TestWatchDebouncerStop_WaitsForInFlightRun(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool

	w := newWatchDebouncer(5*time.Millisecond, func(_ string, _ bool) {
		close(started)
		<-release
		finished.Store(true)
	})

	w.onEvent("root", false)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("run never started")
	}

	stopDone := make(chan struct{})
	go func() {
		w.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop() returned before the in-flight run finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() never returned after the in-flight run finished")
	}
	if !finished.Load() {
		t.Fatal("Stop() returned before run actually completed")
	}
}

func waitForCalls(t *testing.T, mu *sync.Mutex, calls *[]bool, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(*calls)
		mu.Unlock()
		if n >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("run called %d time(s) within 2s, want at least %d", n, want)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestNeedsGraphReload covers needsGraphReload's classification of a .go
// file change: an edit to a file the graph already knows about never needs
// a reload (revalidateIndex's own content hash covers it), but a change
// that alters an existing package's GoFiles list — a known file being
// deleted, or an unknown file landing in an already-known package's
// directory, whether reported as Created or Changed — does.
func TestNeedsGraphReload(t *testing.T) {
	snap := &graph.Snapshot{Packages: map[string]*graph.Package{
		"example.com/mod/pkg": {ImportPath: "example.com/mod/pkg", Dir: "/root/pkg", GoFiles: []string{"/root/pkg/a.go"}},
	}}
	ws := &workspace{snap: snap, fileToPkg: map[string]string{"/root/pkg/a.go": "example.com/mod/pkg"}}
	dirs := packageDirs(snap)

	tests := []struct {
		name string
		ch   protocol.FileEvent
		want bool
	}{
		{"known file edited", protocol.FileEvent{URI: uri.File("/root/pkg/a.go"), Type: protocol.FileChangeTypeChanged}, false},
		{"known file deleted", protocol.FileEvent{URI: uri.File("/root/pkg/a.go"), Type: protocol.FileChangeTypeDeleted}, true},
		{"new file created in known package dir", protocol.FileEvent{URI: uri.File("/root/pkg/b.go"), Type: protocol.FileChangeTypeCreated}, true},
		{"new file changed (no create event) in known package dir", protocol.FileEvent{URI: uri.File("/root/pkg/b.go"), Type: protocol.FileChangeTypeChanged}, true},
		{"file created in a brand-new directory", protocol.FileEvent{URI: uri.File("/root/newpkg/c.go"), Type: protocol.FileChangeTypeCreated}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsGraphReload(ws, dirs, tt.ch); got != tt.want {
				t.Errorf("needsGraphReload(%+v) = %v, want %v", tt.ch, got, tt.want)
			}
		})
	}
}

// TestHandleDidChangeWatchedFiles_KnownGoFileEditSchedulesRevalidate
// verifies the wiring from the notification handler into s.watch: an edit
// to a file already part of the loaded workspace schedules a
// revalidateWorkspace pass with reload=false.
func TestHandleDidChangeWatchedFiles_KnownGoFileEditSchedulesRevalidate(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	knownFile := s.workspace().snap.Packages["example.com/servermod/greet"].GoFiles[0]

	calls := installSpyWatch(s)

	if err := s.handleDidChangeWatchedFiles(context.Background(), mustMarshal(t, &protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri.File(knownFile), Type: protocol.FileChangeTypeChanged}},
	})); err != nil {
		t.Fatalf("handleDidChangeWatchedFiles: %v", err)
	}

	select {
	case c := <-calls:
		if c.root != root || c.reload {
			t.Fatalf("got %+v, want {root:%s reload:false}", c, root)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch was never scheduled for a known .go file edit")
	}
}

// TestHandleDidChangeWatchedFiles_DeletedKnownFileNeedsReload verifies that
// deleting a file the workspace already knows about schedules a
// revalidateWorkspace pass with reload=true.
func TestHandleDidChangeWatchedFiles_DeletedKnownFileNeedsReload(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	knownFile := s.workspace().snap.Packages["example.com/servermod/greet"].GoFiles[0]

	calls := installSpyWatch(s)

	if err := s.handleDidChangeWatchedFiles(context.Background(), mustMarshal(t, &protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri.File(knownFile), Type: protocol.FileChangeTypeDeleted}},
	})); err != nil {
		t.Fatalf("handleDidChangeWatchedFiles: %v", err)
	}

	select {
	case c := <-calls:
		if !c.reload {
			t.Fatalf("got %+v, want reload:true", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch was never scheduled for a deleted known .go file")
	}
}

// TestHandleDidChangeWatchedFiles_NonGoFileIsIgnored verifies that a
// non-.go file change (e.g. a README) never schedules a revalidation pass.
func TestHandleDidChangeWatchedFiles_NonGoFileIsIgnored(t *testing.T) {
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	calls := installSpyWatch(s)

	if err := s.handleDidChangeWatchedFiles(context.Background(), mustMarshal(t, &protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri.File(filepath.Join(root, "README.md")), Type: protocol.FileChangeTypeChanged}},
	})); err != nil {
		t.Fatalf("handleDidChangeWatchedFiles: %v", err)
	}

	select {
	case c := <-calls:
		t.Fatalf("watch was scheduled for a non-.go file change: %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

// watchCall records one s.watch run invocation, for installSpyWatch.
type watchCall struct {
	root   string
	reload bool
}

// installSpyWatch replaces s.watch with a fast-debouncing watchDebouncer
// that records every call it would otherwise have made to
// revalidateWorkspace, instead of actually calling it (which would launch a
// real `go list`/indexer subprocess) — for tests that only need to assert
// on handleDidChangeWatchedFiles's classification and scheduling, not on
// revalidateWorkspace itself.
func installSpyWatch(s *Server) <-chan watchCall {
	calls := make(chan watchCall, 8)
	s.watch = newWatchDebouncer(5*time.Millisecond, func(root string, reload bool) {
		calls <- watchCall{root, reload}
	})
	return calls
}

// TestRevalidateWorkspace_ReloadPicksUpNewPackage verifies the reload=true
// path end to end: a brand-new package added to an already-loaded
// workspace is picked up once revalidateWorkspace reloads the import
// graph, without restarting the server.
func TestRevalidateWorkspace_ReloadPicksUpNewPackage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := copyTestdataModule(t)

	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	s := newWorkspaceOnlyServerAt(t, root, snap)

	if _, ok := snap.Packages["example.com/servermod/newpkg"]; ok {
		t.Fatal("newpkg already present before it was created; test setup is wrong")
	}

	newDir := filepath.Join(root, "newpkg")
	if err := os.MkdirAll(newDir, 0o750); err != nil {
		t.Fatalf("mkdir newpkg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "newpkg.go"), []byte("package newpkg\n"), 0o600); err != nil {
		t.Fatalf("write newpkg.go: %v", err)
	}

	s.revalidateWorkspace(root, true)

	if _, ok := s.workspace().snap.Packages["example.com/servermod/newpkg"]; !ok {
		t.Fatal("revalidateWorkspace(reload=true) did not pick up the new package")
	}
}

// copyTestdataModule copies testdata/module into a fresh t.TempDir(), so a
// test that writes new files into the workspace root never mutates the
// checked-in testdata.
func copyTestdataModule(t *testing.T) string {
	t.Helper()
	srcPath, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	src, err := os.OpenRoot(srcPath)
	if err != nil {
		t.Fatalf("open root %s: %v", srcPath, err)
	}
	defer func() { _ = src.Close() }()

	dstPath := t.TempDir()
	dst, err := os.OpenRoot(dstPath)
	if err != nil {
		t.Fatalf("open root %s: %v", dstPath, err)
	}
	defer func() { _ = dst.Close() }()

	entries, err := os.ReadDir(srcPath)
	if err != nil {
		t.Fatalf("read testdata module: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // testdata/module is flat today; nothing to recurse into
		}
		data, err := src.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := dst.WriteFile(e.Name(), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}
	if err := dst.MkdirAll("greet", 0o750); err != nil {
		t.Fatalf("mkdir greet: %v", err)
	}
	greetRelPath := filepath.Join("greet", "greet.go")
	greetSrc, err := src.ReadFile(greetRelPath)
	if err != nil {
		t.Fatalf("read greet/greet.go: %v", err)
	}
	if err := dst.WriteFile(greetRelPath, greetSrc, 0o600); err != nil {
		t.Fatalf("write greet/greet.go: %v", err)
	}
	return dstPath
}

// newWorkspaceOnlyServerAt is newWorkspaceOnlyServer parameterized on root
// and snap, for tests exercising a copied (mutable) workspace rather than
// the shared read-only testdata/module.
func newWorkspaceOnlyServerAt(t *testing.T, root string, snap *graph.Snapshot) *Server {
	t.Helper()
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	return s
}
