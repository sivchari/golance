package check

import (
	"context"
	"errors"
	"go/types"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/overlay"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// countingImporterHook wraps an Importer factory to count how many times it
// is called — a precise proxy for "how many rechecks actually ran" (see
// Importer's doc: Engine calls it exactly once per recheck), mirroring
// depcheck.TestProvider_Singleflight's countingMetadataSource.
func countingImporterHook(checks *int64) func(Importer) Importer {
	return func(imp Importer) Importer {
		return func() types.ImporterFrom {
			atomic.AddInt64(checks, 1)
			return imp()
		}
	}
}

// TestEngine_Get_ConcurrentSameContentSingleflight covers task 1 of the
// singleflight fix: many concurrent Gets for the same (unitKey,
// contentHash) collapse onto a single underlying recheck instead of each
// racing a redundant one.
func TestEngine_Get_ConcurrentSameContentSingleflight(t *testing.T) {
	var checks int64
	e, root := newTestEngineWithImporterHook(t, overlay.New(), Options{}, countingImporterHook(&checks))
	path := filepath.Join(root, "basic", "basic.go")
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	results := make([]*CheckedPackage, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = e.Get(ctx, path)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Get: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Errorf("goroutine %d got a different *CheckedPackage than goroutine 0", i)
		}
	}
	if got := atomic.LoadInt64(&checks); got != 1 {
		t.Errorf("newImporter called %d times, want exactly 1 (singleflight should collapse %d concurrent Gets)", got, n)
	}
}

// TestEngine_Get_CanceledWaiterDoesNotKillFlight covers the core of the
// fix: a Get whose own ctx is canceled while its flight is still mid-check
// must return promptly, but the flight itself must keep running and commit
// to the cache — so a subsequent Get is served from the warm cache rather
// than re-running the check. The importer factory (called exactly once per
// recheck) is gated so the first call blocks until released, holding the
// flight "in flight" past the point where a waiter's ctx cancellation is
// observable.
func TestEngine_Get_CanceledWaiterDoesNotKillFlight(t *testing.T) {
	var checks int64
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hook := func(imp Importer) Importer {
		return func() types.ImporterFrom {
			atomic.AddInt64(&checks, 1)
			once.Do(func() {
				close(started)
				<-release
			})
			return imp()
		}
	}
	e, root := newTestEngineWithImporterHook(t, overlay.New(), Options{}, hook)
	path := filepath.Join(root, "basic", "basic.go")

	ctx, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := e.Get(ctx, path)
		firstErr <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("flight never reached the importer factory")
	}

	cancel()
	select {
	case err := <-firstErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Get returned err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get with a canceled ctx never returned despite the flight still being blocked")
	}

	close(release) // unblock the flight; it must run to completion and commit

	cp, err := e.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get after the flight completed: %v", err)
	}
	if cp == nil {
		t.Fatal("Get returned a nil CheckedPackage with a nil error")
	}
	if got := atomic.LoadInt64(&checks); got != 1 {
		t.Errorf("newImporter called %d times, want exactly 1 (the canceled waiter must not have killed the flight or triggered a second check)", got)
	}
}

// TestEngine_Get_EditMidFlightStartsFreshFlight covers getOrStartFlight's
// hash check: an edit that lands while a flight for the previous content is
// still running must start its own fresh flight rather than join the stale
// one, and the stale flight's eventual commit must not clobber the fresh
// one's — the same generation guard that already orders a request-driven
// and a background recheck (see commit's doc), now exercised by two
// concurrently running flights for the same unit.
func TestEngine_Get_EditMidFlightStartsFreshFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var gated int32
	hook := func(imp Importer) Importer {
		return func() types.ImporterFrom {
			// CAS, not sync.Once: the second flight's own call into this
			// hook must proceed immediately rather than blocking behind the
			// first flight's still-running gate (sync.Once.Do blocks
			// concurrent callers until the winning call returns, which
			// would deadlock the two flights this test deliberately runs
			// concurrently against each other).
			if atomic.CompareAndSwapInt32(&gated, 0, 1) {
				close(started)
				<-release
			}
			return imp()
		}
	}
	ov := overlay.New()
	e, root := newTestEngineWithImporterHook(t, ov, Options{}, hook)
	path := filepath.Join(root, "basic", "basic.go")
	dir := filepath.Dir(path)
	key := unitKey{dir: dir, variant: variantBase}

	type outcome struct {
		cp  *CheckedPackage
		err error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		cp, err := e.Get(context.Background(), path)
		firstDone <- outcome{cp, err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first flight never reached the importer factory")
	}

	// Edit the file while the first flight is still blocked mid-check: a
	// different contentHash for the same unit.
	ov.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI:        uri.File(path),
		LanguageID: "go",
		Version:    1,
		Text:       "package basic\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Sub(a, b int) int { return a - b }\n",
	}})

	cp2, err := e.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get after the mid-flight edit: %v", err)
	}
	if cp2.Package().Scope().Lookup("Sub") == nil {
		t.Error("Sub, declared only in the edited content, not found in the second flight's package scope")
	}

	close(release) // let the stale first flight finish and attempt its (losing) commit

	var first outcome
	select {
	case first = <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first flight's Get never returned after being unblocked")
	}
	if first.err != nil {
		t.Fatalf("first flight's Get: %v", first.err)
	}
	if first.cp.Package().Scope().Lookup("Sub") != nil {
		t.Error("first flight's own CheckedPackage should reflect the pre-edit content (no Sub), got the edited content")
	}

	e.mu.Lock()
	cached := e.cache[key]
	e.mu.Unlock()
	if cached == nil || cached.pkg != cp2 {
		t.Fatalf("cache holds %+v, want the second (newer) flight's result — the stale first flight's commit must not have clobbered it", cached)
	}
}

// TestEngine_Stop_CancelsInFlightFlight covers requirement 3: Engine.Stop
// must cancel a request-driven flight already in progress, not just
// debounce-triggered background rechecks, so a server shutdown can reclaim
// every goroutine it started. Get itself is called with a ctx that is never
// canceled (context.Background), so the only way it can return is via its
// flight's own completion — proving Stop reached the flight, not just the
// waiter.
func TestEngine_Stop_CancelsInFlightFlight(t *testing.T) {
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

	e.Stop()
	close(release) // unblock the now-canceled flight; it should notice e.ctx and bail

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get after Stop returned err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get never returned after Stop canceled its flight (possible goroutine leak)")
	}
}
