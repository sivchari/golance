package server

import (
	"os"
	"sync"
	"time"

	"go.lsp.dev/protocol"
)

// defaultWatchDebounce is the Options.WatchDebounce default: how long
// watchDebouncer waits for workspace/didChangeWatchedFiles notifications to
// go quiet before running a revalidation pass. A `git pull` or `git
// checkout` reports every touched file as its own FileEvent, arriving as
// fast as the client's own file watcher can report them — long enough to
// coalesce a burst like that into one pass, short enough that a change made
// by an external tool (a codegen step, `gofmt -w` from a Makefile, ...)
// still shows up in a reasonable time.
const defaultWatchDebounce = 750 * time.Millisecond

// watchDebouncer coalesces bursts of workspace/didChangeWatchedFiles
// notifications for .go files into a single revalidateWorkspace pass.
//
// onEvent is a true debounce, not a throttle: every call restarts the
// timer, so it never fires while events keep arriving. Once it does fire,
// the pass it runs is serialized against any pass already in flight via a
// singleflight-with-one-pending-rerun scheme: a revalidation already
// running when the timer fires is left alone, and exactly one more pass
// runs immediately after it finishes, folding in whatever arrived in the
// meantime. run — s.revalidateWorkspace — can take as long as the indexer
// subprocess's full rebuild, so this guarantees it never runs twice
// concurrently and is never more than one pass behind.
type watchDebouncer struct {
	run   func(root string, reload bool)
	delay time.Duration

	mu     sync.Mutex
	timer  *time.Timer
	root   string // accumulated across the current debounce window
	reload bool   // accumulated (OR'd) across the current debounce window

	// execMu guards the fields below, which together implement the
	// singleflight-with-one-pending-rerun scheme described above.
	execMu      sync.Mutex
	running     bool
	rerun       bool
	rerunRoot   string
	rerunReload bool

	// runWG tracks fire's in-flight run loop (one running=true..false span
	// at a time, regardless of how many reruns it folds in), so Stop can
	// wait for it to actually finish instead of merely preventing a new one
	// from starting.
	runWG sync.WaitGroup
}

// newWatchDebouncer returns a watchDebouncer that calls run for each
// coalesced batch of events. delay <= 0 uses defaultWatchDebounce.
func newWatchDebouncer(delay time.Duration, run func(root string, reload bool)) *watchDebouncer {
	if delay <= 0 {
		delay = defaultWatchDebounce
	}
	return &watchDebouncer{run: run, delay: delay}
}

// onEvent records that a batch of .go file changes for root arrived —
// reload reports whether any of them can only be resolved by reloading the
// import graph (see needsGraphReload) — accumulates it (OR'd) against
// whatever else has arrived since the last fire, and (re)starts the
// debounce timer. root is expected constant across a session (the
// workspace root never changes once loaded); the last value wins if it
// somehow were not.
func (w *watchDebouncer) onEvent(root string, reload bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.root = root
	w.reload = w.reload || reload
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.delay, w.fire)
}

// fire runs, or schedules, one revalidation pass over whatever onEvent has
// accumulated since the last one. It always runs on the debounce timer's
// own goroutine, never the notification handler that called onEvent, so a
// slow revalidation (a full indexer subprocess rebuild) never blocks LSP
// request/notification dispatch.
func (w *watchDebouncer) fire() {
	w.mu.Lock()
	root, reload := w.root, w.reload
	w.reload = false
	w.mu.Unlock()

	w.execMu.Lock()
	if w.running {
		w.rerun = true
		w.rerunRoot = root
		w.rerunReload = w.rerunReload || reload
		w.execMu.Unlock()
		return
	}
	w.running = true
	w.runWG.Add(1)
	w.execMu.Unlock()
	defer w.runWG.Done()

	for {
		w.run(root, reload)

		w.execMu.Lock()
		if !w.rerun {
			w.running = false
			w.execMu.Unlock()
			return
		}
		root, reload = w.rerunRoot, w.rerunReload
		w.rerun, w.rerunReload = false, false
		w.execMu.Unlock()
	}
}

// Stop cancels w's pending debounce timer, if any, so it never fires after
// the caller no longer wants it to (server shutdown), then blocks until
// any run loop already in flight finishes — which, since run (see
// Server.revalidateWorkspace) is expected to observe the same shutdown
// signal via context cancellation, should be prompt rather than a wait for
// a full rebuild. Safe to call more than once.
func (w *watchDebouncer) Stop() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
	w.runWG.Wait()
}

// fileFingerprint is a cheap (size, mtime) snapshot of a file's on-disk
// state, used by watchFingerprints to recognize a workspace/didChangeWatchedFiles
// event that reports no genuine change.
type fileFingerprint struct {
	size    int64
	modTime int64
}

// watchFingerprints remembers, per path, the fileFingerprint last observed
// by a non-suppressed handleDidChangeWatchedFiles event, so a later
// notification reporting the exact same (size, mtime) for that path — the
// shape of an editor re-sending a periodic no-op watched-files batch, e.g. a
// filesystem watcher that polls and reports mtime-identical files as
// "changed" — can be recognized and skipped before it ever reaches
// s.watch/revalidateWorkspace's workspace-wide revalidation fan-out.
//
// It deliberately remembers nothing about a path before the first event
// handleDidChangeWatchedFiles has actually seen for it: the workspace's
// initial graph.Snapshot is never consulted to pre-populate this, so the
// very first notification for any path is always treated as a real change
// (matching the behavior handleDidChangeWatchedFiles already had before
// this existed) — only a *repeat* notification reporting an identical
// on-disk (size, mtime) is ever suppressed.
type watchFingerprints struct {
	mu   sync.Mutex
	seen map[string]fileFingerprint
}

// newWatchFingerprints returns an empty watchFingerprints.
func newWatchFingerprints() *watchFingerprints {
	return &watchFingerprints{seen: make(map[string]fileFingerprint)}
}

// changed reports whether the event described by (path, typ) represents a
// genuine change worth acting on — false only when a prior call already
// recorded the exact same (size, mtime) for path and typ is not a deletion.
// As a side effect it updates what is remembered for path: a deletion
// forgets it entirely (so a file later re-created at the same path is
// compared against nothing, i.e. treated as a real change again, rather
// than against stale pre-deletion stat data), and any other event records
// path's current on-disk (size, mtime). If the stat itself fails (a
// created-then-immediately-deleted file racing this call, for instance)
// this conservatively reports a real change without touching what is
// remembered, leaving a later event to settle it once the file's state
// stabilizes.
func (f *watchFingerprints) changed(path string, typ protocol.FileChangeType) bool {
	if typ == protocol.FileChangeTypeDeleted {
		f.mu.Lock()
		delete(f.seen, path)
		f.mu.Unlock()
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return true
	}
	fp := fileFingerprint{size: fi.Size(), modTime: fi.ModTime().UnixNano()}

	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.seen[path]; ok && old == fp {
		return false
	}
	f.seen[path] = fp
	return true
}
