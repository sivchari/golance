package server

import (
	"sync"
	"time"
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
	w.execMu.Unlock()

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
