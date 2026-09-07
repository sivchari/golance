package check

import (
	"context"
	"go/types"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sivchari/golance/internal/overlay"
)

// TestEngine_Retire_LetsInFlightGetFinish covers Retire's core contract,
// the opposite of TestEngine_Stop_CancelsInFlightFlight: Retire must NOT
// cancel a request-driven flight already in progress, so its waiter gets
// back a real result instead of ctx.Err(). Get is called with
// context.Background, so the only way it can return is via its flight's own
// completion.
func TestEngine_Retire_LetsInFlightGetFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		hook := func(imp Importer) Importer {
			return func() types.ImporterFrom {
				once.Do(func() {
					close(started)
					<-release
				})
				return imp()
			}
		}
		e, root := newTestEngineWithImporterHook(t, overlay.New(), Options{}, hook)
		path := filepath.Join(root, "basic", "basic.go")

		done := make(chan struct {
			cp  *CheckedPackage
			err error
		}, 1)
		go func() {
			cp, err := e.Get(context.Background(), path)
			done <- struct {
				cp  *CheckedPackage
				err error
			}{cp, err}
		}()

		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("flight never reached the importer factory")
		}

		e.Retire()
		close(release) // unblock the flight; Retire must not have canceled e.ctx

		select {
		case res := <-done:
			if res.err != nil {
				t.Fatalf("Get after Retire returned err = %v, want nil", res.err)
			}
			if res.cp == nil {
				t.Fatal("Get after Retire returned a nil CheckedPackage with a nil error")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Get never returned after Retire; the flight may have been wrongly canceled")
		}
	})
}

// TestEngine_Retire_SuppressesOnResult covers the other half of Retire's
// contract: a recheck that commits after Retire must not reach
// Options.OnResult, even though its cache write (and its own Get waiter, see
// TestEngine_Retire_LetsInFlightGetFinish) still succeed normally.
func TestEngine_Retire_SuppressesOnResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		hook := func(imp Importer) Importer {
			return func() types.ImporterFrom {
				once.Do(func() {
					close(started)
					<-release
				})
				return imp()
			}
		}
		var onResultCalls int64
		e, root := newTestEngineWithImporterHook(t, overlay.New(), Options{
			OnResult: func(*Result) { atomic.AddInt64(&onResultCalls, 1) },
		}, hook)
		path := filepath.Join(root, "basic", "basic.go")

		done := make(chan error, 1)
		go func() {
			_, err := e.Get(context.Background(), path)
			done <- err
		}()

		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("flight never reached the importer factory")
		}

		e.Retire()
		close(release)

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Get after Retire: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Get never returned after Retire")
		}

		if got := atomic.LoadInt64(&onResultCalls); got != 0 {
			t.Errorf("OnResult called %d times for a recheck that committed after Retire, want 0", got)
		}
	})
}

// TestEngine_Retire_CancelsPendingDebounceTimer covers the debounce half of
// Retire, mirroring TestEngine_Stop_CancelsPendingDebounceTimer: a debounce
// timer armed before Retire must never fire afterward.
func TestEngine_Retire_CancelsPendingDebounceTimer(t *testing.T) {
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
		e.Retire()
		time.Sleep(150 * time.Millisecond) // long enough for the debounce delay to have elapsed, if it were still armed

		mu.Lock()
		got := count
		mu.Unlock()
		if got != 0 {
			t.Fatalf("OnResult called %d times after Retire, want 0 (the pending debounce timer must never fire)", got)
		}
	})
}

// TestEngine_Retire_CancelsInFlightBackgroundRecheck mirrors
// TestEngine_Stop_CancelsInFlightBackgroundRecheck: a debounce-triggered
// background recheck already in flight when Retire is called must be
// canceled before it can publish.
func TestEngine_Retire_CancelsInFlightBackgroundRecheck(t *testing.T) {
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

		e.Retire()
		close(gr.release) // unblock the now-canceled job; it should notice and bail
		time.Sleep(150 * time.Millisecond)

		mu.Lock()
		got := count
		mu.Unlock()
		if got != 0 {
			t.Fatalf("OnResult called %d times after Retire, want 0 (the in-flight background job must be canceled before it can publish)", got)
		}
	})
}
