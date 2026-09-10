package server

import (
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
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

// maxFingerprintHashSize bounds how large a file watchFingerprints.changed
// will read to compute fileFingerprint.hash. (size, mtime) alone is
// ambiguous on a coarse-mtime filesystem, or against a codegen tool that
// writes deterministic timestamps: a real edit can land on the exact same
// (size, mtime) an earlier event for the same path already recorded, making
// it indistinguishable from a genuine no-op resend without also comparing
// content. Reading and hashing the whole file settles that for anything
// this size or smaller; above it, changed conservatively reports a change
// instead of paying for a large read on every event (this runs per watched
// file, but a `git pull`/branch switch can report thousands of them in one
// burst).
const maxFingerprintHashSize = 1 << 20 // 1 MiB

// fileFingerprint is a (size, mtime, content hash) snapshot of a file's
// on-disk state, used by watchFingerprints to recognize a
// workspace/didChangeWatchedFiles event that reports no genuine change.
// hashed reports whether hash was actually computed (size was within
// maxFingerprintHashSize and the file was readable) — false makes hash
// meaningless rather than a false zero-value match.
type fileFingerprint struct {
	size    int64
	modTime int64
	hash    uint64
	hashed  bool
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
// genuine change worth acting on. As a side effect it updates what is
// remembered for path: a deletion forgets it entirely (so a file later
// re-created at the same path is compared against nothing, i.e. treated as
// a real change again, rather than against stale pre-deletion stat data),
// and any other event records path's current fileFingerprint. If the stat
// itself fails (a created-then-immediately-deleted file racing this call,
// for instance) this conservatively reports a real change without touching
// what is remembered, leaving a later event to settle it once the file's
// state stabilizes.
//
// A prior call having recorded the exact same (size, mtime) for path is not
// on its own enough to report no change: on a coarse-mtime filesystem, or
// against a codegen tool that writes deterministic timestamps, a genuine
// edit can land on that same (size, mtime) too. Whenever path is
// maxFingerprintHashSize or smaller, a content hash — computed on every
// call, not just an ambiguous one, so two fingerprints are always directly
// comparable — settles it; a larger file has none and any (size, mtime)
// match for it is trusted as-is, the same way this whole check worked
// before hashing existed.
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
	if fp.size <= maxFingerprintHashSize {
		if h, ok := hashFile(path); ok {
			fp.hash, fp.hashed = h, true
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.seen[path]
	f.seen[path] = fp
	if !ok || old.size != fp.size || old.modTime != fp.modTime {
		return true
	}
	if fp.hashed && old.hashed {
		return fp.hash != old.hash
	}
	return true
}

// hashFile returns an FNV-1a content hash of path, or ok=false if it could
// not be read in full (a race with the file being deleted or truncated
// between the caller's own os.Stat and this call, for instance) — changed's
// caller treats that the same as never having hashed it, conservatively
// reporting a change.
func hashFile(path string) (sum uint64, ok bool) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	h := fnv.New64a()
	if _, err := io.Copy(h, f); err != nil {
		return 0, false
	}
	return h.Sum64(), true
}
