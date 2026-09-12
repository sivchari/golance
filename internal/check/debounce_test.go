package check

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sivchari/golance/internal/overlay"
)

// TestEngine_Invalidate_Debounces covers (d): a burst of Invalidate calls
// for the same directory collapses into a single recheck.
func TestEngine_Invalidate_Debounces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var count int

		e, root := newTestEngine(t, overlay.New(), Options{
			DebounceDelay: 20 * time.Millisecond,
			OnResult: func(*Result) {
				mu.Lock()
				count++
				mu.Unlock()
			},
		})
		dir := filepath.Join(root, "debounce")

		for range 5 {
			e.Invalidate(dir)
		}

		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		got := count
		mu.Unlock()
		if got != 1 {
			t.Errorf("OnResult called %d times, want 1", got)
		}
	})
}

// gatingReader wraps a FileReader so its very first ReadFile call signals
// started and then blocks until release is closed. Later calls pass
// through untouched. Used to hold a recheck job "in flight" long enough to
// observe cancellation. Gating is CAS-based, not sync.Once: a concurrent
// second recheck's own call into ReadFile must pass through immediately
// rather than blocking behind the first call's still-running gate (unlike a
// channel receive, sync.Once.Do's internal mutex is not durably blocking
// under testing/synctest — see TestEngine_Get_EditMidFlightStartsFreshFlight
// in flight_test.go for the same tradeoff).
type gatingReader struct {
	overlay.FileReader
	gated   int32
	started chan struct{}
	release chan struct{}
}

func (g *gatingReader) ReadFile(path string) ([]byte, error) {
	if atomic.CompareAndSwapInt32(&g.gated, 0, 1) {
		close(g.started)
		<-g.release
	}
	return g.FileReader.ReadFile(path)
}

// TestEngine_Invalidate_CancelsInFlightRecheck covers (e): a recheck still
// running when a later debounce fires is canceled before the next one
// starts, so only the superseding recheck's result is published.
func TestEngine_Invalidate_CancelsInFlightRecheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gr := &gatingReader{
			FileReader: overlay.New(),
			started:    make(chan struct{}),
			release:    make(chan struct{}),
		}
		var mu sync.Mutex
		var count int
		var last *Result

		e, root := newTestEngine(t, gr, Options{
			DebounceDelay: 20 * time.Millisecond,
			OnResult: func(r *Result) {
				mu.Lock()
				count++
				last = r
				mu.Unlock()
			},
		})
		dir := filepath.Join(root, "debounce")

		e.Invalidate(dir)
		select {
		case <-gr.started:
		case <-time.After(2 * time.Second):
			t.Fatal("first recheck never started")
		}

		// The first job is now blocked mid-flight. A second Invalidate should
		// cancel it once its own debounce elapses.
		e.Invalidate(dir)
		time.Sleep(150 * time.Millisecond) // let the second debounce fire and job2 finish

		close(gr.release) // unblock job1; it should notice cancellation and bail
		time.Sleep(150 * time.Millisecond)

		mu.Lock()
		gotCount := count
		res := last
		mu.Unlock()

		if gotCount != 1 {
			t.Fatalf("OnResult called %d times, want exactly 1 (the canceled job must not publish)", gotCount)
		}
		if res == nil || res.Dir != dir {
			t.Fatalf("unexpected result: %+v", res)
		}
	})
}

// flakyReader wraps a FileReader so its first n ReadFile calls (across every
// path) fail with a transient-looking error, and every call after that
// passes straight through — modeling a directory read racing an external
// file rewrite (git checkout, an editor's atomic save) that clears up on its
// own by the time a retry reads it again.
type flakyReader struct {
	overlay.FileReader
	remaining int32
}

func (f *flakyReader) ReadFile(path string) ([]byte, error) {
	if atomic.AddInt32(&f.remaining, -1) >= 0 {
		return nil, fmt.Errorf("flakyReader: transient read failure for %s", path)
	}
	return f.FileReader.ReadFile(path)
}

// TestEngine_FireRecheck_RetriesTransientReadFailure is a regression test
// for Finding H6: a debounce-triggered background recheck (fireRecheck) that
// hits a transient read failure used to discard runRecheck's result outright
// and never retry, leaving diagnostics frozen with nothing left to
// retrigger a check for the directory. Two failures are enough to fail both
// of canonicalPackageName's own attempts (the known-goFiles probe and its
// candidates fallback) within a single runRecheck call, forcing resolveFiles
// itself to fail — the same shape a real racing rewrite produces.
func TestEngine_FireRecheck_RetriesTransientReadFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader := &flakyReader{FileReader: overlay.New(), remaining: 2}
		var mu sync.Mutex
		var count int

		e, root := newTestEngine(t, reader, Options{
			DebounceDelay: 20 * time.Millisecond,
			OnResult: func(*Result) {
				mu.Lock()
				count++
				mu.Unlock()
			},
		})
		dir := filepath.Join(root, "debounce")

		e.Invalidate(dir)
		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		got := count
		mu.Unlock()
		if got != 1 {
			t.Fatalf("OnResult called %d times, want 1 (a transient read failure must be retried, not silently dropped)", got)
		}
	})
}

// TestEngine_Stop_CancelsInFlightBackgroundRecheck covers the fix for
// Finding 5: a debounce-triggered background recheck already in flight when
// the caller (e.g. Server.setWorkspace, discarding this Engine for a fresh
// one over a new import graph) calls Stop must not go on to publish via
// OnResult afterward.
func TestEngine_Stop_CancelsInFlightBackgroundRecheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gr := &gatingReader{
			FileReader: overlay.New(),
			started:    make(chan struct{}),
			release:    make(chan struct{}),
		}
		var mu sync.Mutex
		var count int

		e, root := newTestEngine(t, gr, Options{
			DebounceDelay: 20 * time.Millisecond,
			OnResult: func(*Result) {
				mu.Lock()
				count++
				mu.Unlock()
			},
		})
		dir := filepath.Join(root, "debounce")

		e.Invalidate(dir)
		select {
		case <-gr.started:
		case <-time.After(2 * time.Second):
			t.Fatal("background recheck never started")
		}

		e.Stop()
		close(gr.release) // unblock the now-canceled job; it should notice and bail
		time.Sleep(150 * time.Millisecond)

		mu.Lock()
		got := count
		mu.Unlock()
		if got != 0 {
			t.Fatalf("OnResult called %d times after Stop, want 0 (the in-flight job must be canceled before it can publish)", got)
		}
	})
}

// TestEngine_Wait_ReturnsPromptlyAfterStopCancelsPendingTimer is a
// regression test for a debounceWG accounting bug: Stop successfully
// canceling a debounce timer that had not yet fired must balance the Add
// armDebounceLocked made for it, or Wait — a caller's only way to block
// until no in-flight recheck can still be touching disk, see Wait's own
// doc — hangs forever for the overwhelmingly common case (a test whose
// debounce delay never actually elapses before cleanup runs Stop).
func TestEngine_Wait_ReturnsPromptlyAfterStopCancelsPendingTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, root := newTestEngine(t, overlay.New(), Options{DebounceDelay: 200 * time.Millisecond})
		dir := filepath.Join(root, "debounce")

		e.Invalidate(dir)
		e.Stop()

		done := make(chan struct{})
		go func() {
			e.Wait()
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Wait() did not return once every bubble goroutine settled; Stop canceling a never-fired timer left debounceWG's counter unbalanced")
		}
	})
}

// TestEngine_Wait_BlocksUntilInFlightRecheckActuallyFinishes covers the
// other half of Wait's contract: it must still block for a recheck that
// had already started (past armDebounceLocked, mid-runRecheck) when Stop
// canceled it, until that goroutine actually returns — not just until
// Stop's own call completes, which per Stop's own doc cannot make that
// happen instantaneously.
func TestEngine_Wait_BlocksUntilInFlightRecheckActuallyFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gr := &gatingReader{
			FileReader: overlay.New(),
			started:    make(chan struct{}),
			release:    make(chan struct{}),
		}
		e, root := newTestEngine(t, gr, Options{DebounceDelay: 20 * time.Millisecond})
		dir := filepath.Join(root, "debounce")

		e.Invalidate(dir)
		select {
		case <-gr.started:
		case <-time.After(2 * time.Second):
			t.Fatal("background recheck never started")
		}

		e.Stop()

		done := make(chan struct{})
		go func() {
			e.Wait()
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Wait() returned before the in-flight recheck's gated read was ever released")
		default:
		}

		close(gr.release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Wait() never returned after the in-flight recheck finished")
		}
	})
}

// TestEngine_Stop_CancelsPendingDebounceTimer covers the other half of
// Finding 5's fix: a debounce timer that has not fired yet must never fire
// after Stop.
func TestEngine_Stop_CancelsPendingDebounceTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var count int

		e, root := newTestEngine(t, overlay.New(), Options{
			DebounceDelay: 20 * time.Millisecond,
			OnResult: func(*Result) {
				mu.Lock()
				count++
				mu.Unlock()
			},
		})
		dir := filepath.Join(root, "debounce")

		e.Invalidate(dir)
		e.Stop()
		time.Sleep(150 * time.Millisecond) // long enough for the debounce delay to have elapsed, if it were still armed

		mu.Lock()
		got := count
		mu.Unlock()
		if got != 0 {
			t.Fatalf("OnResult called %d times after Stop, want 0 (the pending debounce timer must never fire)", got)
		}
	})
}

// TestEngine_Invalidate_AfterStopNeverPublishes is a regression test for a
// gap TestEngine_Stop_CancelsPendingDebounceTimer and
// TestEngine_Stop_CancelsInFlightBackgroundRecheck do not cover: a debounce
// timer armed (or already fired) so close to Stop that Stop's own
// disarm/cancel loop races it rather than observing it. Invalidate here runs
// entirely after Stop has already returned, so its timer is armed with
// e.ctx already canceled — reproducing, deterministically, the same state a
// timer that fires concurrently with Stop ends up in once its callback
// reaches startJob (see startJob's doc): without startJob's e.ctx check,
// this would still fire after the debounce delay and reach
// commit/Options.OnResult despite Stop having already returned.
func TestEngine_Invalidate_AfterStopNeverPublishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var count int

		e, root := newTestEngine(t, overlay.New(), Options{
			DebounceDelay: 20 * time.Millisecond,
			OnResult: func(*Result) {
				mu.Lock()
				count++
				mu.Unlock()
			},
		})
		dir := filepath.Join(root, "debounce")

		e.Stop()
		e.Invalidate(dir)
		time.Sleep(150 * time.Millisecond) // long enough for the debounce delay to elapse

		mu.Lock()
		got := count
		mu.Unlock()
		if got != 0 {
			t.Fatalf("OnResult called %d times for a debounce armed after Stop, want 0", got)
		}
	})
}

// TestEngine_StartJob_RacesStopAlreadyCanceled directly exercises the race
// startJob's doc describes: a debounce timer's callback reaching startJob
// after Stop has already canceled e.ctx, but before (or without) ever having
// registered a dirState.cancel Stop's own loop could have canceled instead.
// time.Timer.Stop cannot prevent an already-fired callback from running, so
// this interleaving cannot be forced deterministically through the real
// timer; calling startJob directly, exactly as fireRecheck would from
// inside that callback, exercises the same code path without depending on
// goroutine scheduling.
func TestEngine_StartJob_RacesStopAlreadyCanceled(t *testing.T) {
	var mu sync.Mutex
	var count int

	e, root := newTestEngine(t, overlay.New(), Options{
		OnResult: func(*Result) {
			mu.Lock()
			count++
			mu.Unlock()
		},
	})
	dir := filepath.Join(root, "debounce")
	key := unitKey{dir: dir, variant: variantBase}

	e.Stop()

	ctx, finish := e.startJob(context.Background(), key)
	defer finish()
	if ctx.Err() == nil {
		t.Fatal("startJob after Stop returned a live context, want it pre-canceled")
	}

	if _, err := e.runRecheck(ctx, key); err == nil {
		t.Fatal("runRecheck with a post-Stop context succeeded, want it to bail on the canceled context")
	}

	mu.Lock()
	got := count
	mu.Unlock()
	if got != 0 {
		t.Fatalf("OnResult called %d times for a recheck started after Stop, want 0", got)
	}
}

// TestEngine_Stop_SuppressesPublishFromAFlightPastItsLastCheck covers the
// half of Stop's contract cancellation alone cannot deliver: a
// request-driven flight observes e.ctx only at runRecheck's own
// checkpoints, so one already past its last check still reaches commit
// after Stop returns and would publish diagnostics computed against a
// graph the caller has discarded. Driven white-box, by committing a
// genuinely computed result after Stop, because the window between that
// last checkpoint and commit cannot be forced deterministically from
// outside the package.
func TestEngine_Stop_SuppressesPublishFromAFlightPastItsLastCheck(t *testing.T) {
	var mu sync.Mutex
	var count int

	e, root := newTestEngine(t, overlay.New(), Options{
		OnResult: func(*Result) {
			mu.Lock()
			count++
			mu.Unlock()
		},
	})
	dir := filepath.Join(root, "debounce")
	key := unitKey{dir: dir, variant: variantBase}

	cp, err := e.Get(context.Background(), filepath.Join(dir, "debounce.go"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	mu.Lock()
	count = 0 // ignore the warm-up Get's own publish
	mu.Unlock()

	e.Stop()
	e.commit(key, e.nextGen(key), cp)

	mu.Lock()
	got := count
	mu.Unlock()
	if got != 0 {
		t.Fatalf("OnResult called %d times for a result committed after Stop, want 0", got)
	}
}
