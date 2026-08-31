package check

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/overlay"
)

// TestEngine_Get_NotCanceledByBackgroundRecheck covers the core fix: a
// request-driven Get that is still reading content must not be canceled by
// a debounce-triggered background recheck for the same directory firing
// while it is in flight. Before request-driven and background rechecks
// stopped sharing per-dir supersede cancellation, this reproduced the bug
// where an editor request got "context canceled" for a request that was
// still alive.
func TestEngine_Get_NotCanceledByBackgroundRecheck(t *testing.T) {
	gr := &gatingReader{
		FileReader: overlay.New(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	var mu sync.Mutex
	var bgResults int

	e, root := newTestEngine(t, gr, Options{
		DebounceDelay: 20 * time.Millisecond,
		OnResult: func(*Result) {
			mu.Lock()
			bgResults++
			mu.Unlock()
		},
	})
	dir := filepath.Join(root, "debounce")
	path := filepath.Join(dir, "debounce.go")

	type getOutcome struct {
		cp  *CheckedPackage
		err error
	}
	done := make(chan getOutcome, 1)
	go func() {
		cp, err := e.Get(context.Background(), path)
		done <- getOutcome{cp, err}
	}()

	select {
	case <-gr.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Get's recheck never started reading")
	}

	// Get is now blocked mid-read. Trigger a background recheck for the
	// same directory and give its debounce time to fire and complete —
	// under the old shared-cancellation design this would cancel Get.
	e.Invalidate(dir)
	time.Sleep(150 * time.Millisecond)

	close(gr.release)

	var outcome getOutcome
	select {
	case outcome = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Get never completed after being unblocked")
	}

	if outcome.err != nil {
		t.Fatalf("Get returned error %v, want nil (must not be canceled by a concurrent background recheck)", outcome.err)
	}
	if outcome.cp == nil {
		t.Fatal("Get returned a nil CheckedPackage with a nil error")
	}

	mu.Lock()
	got := bgResults
	mu.Unlock()
	if got == 0 {
		t.Error("expected the background recheck to have completed and published a result")
	}
}

// TestEngine_Commit_OlderGenerationDoesNotClobberNewer covers the ordering
// guard in Engine.commit: request-driven and background rechecks for the
// same directory no longer cancel each other, so they can complete out of
// order. A slower, older-generation commit arriving after a faster,
// newer-generation one must not overwrite the cache or publish a stale
// result via OnResult.
func TestEngine_Commit_OlderGenerationDoesNotClobberNewer(t *testing.T) {
	var mu sync.Mutex
	var results []*Result

	e, root := newTestEngine(t, overlay.New(), Options{
		OnResult: func(r *Result) {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		},
	})
	dir := filepath.Join(root, "basic")

	genOld := e.nextGen(dir)
	genNew := e.nextGen(dir)

	oldCP := &CheckedPackage{pkgPath: "old", dir: dir, builtAt: time.Now()}
	newCP := &CheckedPackage{pkgPath: "new", dir: dir, builtAt: time.Now()}

	// The newer generation commits first (it started reading content
	// later but finished sooner); the older generation commits after.
	e.commit(dir, genNew, newCP)
	e.commit(dir, genOld, oldCP)

	e.mu.Lock()
	cached := e.cache[dir]
	e.mu.Unlock()
	if cached == nil || cached.pkg != newCP {
		t.Fatalf("cache holds %+v, want the newer commit (%q)", cached, newCP.pkgPath)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("OnResult called %d time(s), want 1 (the stale commit must not publish)", len(results))
	}
	if results[0].PkgPath != "new" {
		t.Errorf("published result PkgPath = %q, want %q", results[0].PkgPath, "new")
	}
}

// TestEngine_CommitPublish_OrderedByGeneration pins the exact race the
// cache-write generation guard alone does not prevent: an older-generation
// commit that has already passed commitCache's gate — as it would if it
// started reading content before the newer generation — but is then
// descheduled before reaching commitPublish (e.g. while Diagnostics, which
// reads files, is still computing outside e.mu) must not publish after a
// newer-generation commit that raced ahead and published first. It drives
// commitCache and commitPublish directly (rather than commit, which runs
// both back-to-back in one goroutine) to force that exact interleaving
// deterministically.
func TestEngine_CommitPublish_OrderedByGeneration(t *testing.T) {
	var mu sync.Mutex
	var order []string

	e, root := newTestEngine(t, overlay.New(), Options{
		OnResult: func(r *Result) {
			mu.Lock()
			order = append(order, r.PkgPath)
			mu.Unlock()
		},
	})
	dir := filepath.Join(root, "basic")

	genOld := e.nextGen(dir)
	genNew := e.nextGen(dir)

	oldCP := &CheckedPackage{pkgPath: "old", dir: dir, builtAt: time.Now()}
	newCP := &CheckedPackage{pkgPath: "new", dir: dir, builtAt: time.Now()}

	// genOld passes the cache gate first (it is the older generation).
	stOld, ok := e.commitCache(dir, genOld, oldCP)
	if !ok {
		t.Fatal("commitCache(genOld) rejected, want accepted (first commit for dir)")
	}

	// genOld is now "descheduled" between commitCache and commitPublish.
	// genNew runs to completion, including its publish, before it resumes.
	stNew, ok := e.commitCache(dir, genNew, newCP)
	if !ok {
		t.Fatal("commitCache(genNew) rejected, want accepted (newer than doneGen)")
	}
	e.commitPublish(genNew, stNew, newCP)

	// genOld resumes and reaches commitPublish last — its delayed publish
	// must be dropped, not delivered after genNew's.
	e.commitPublish(genOld, stOld, oldCP)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 {
		t.Fatalf("OnResult calls = %v, want exactly one call, for the newer generation (the delayed, stale publish must be dropped)", order)
	}
	if order[0] != "new" {
		t.Errorf("published PkgPath = %q, want %q", order[0], "new")
	}
}

// TestEngine_Get_HonorsRequestContext covers the one thing that can still
// cancel a request-driven Get: its own ctx. The rpc layer derives this ctx
// per request and cancels it on $/cancelRequest or a dropped connection
// (see internal/rpc/server.go), independent of per-dir job bookkeeping.
func TestEngine_Get_HonorsRequestContext(t *testing.T) {
	e, root := newTestEngine(t, overlay.New(), Options{})
	path := filepath.Join(root, "basic", "basic.go")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.Get(ctx, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get with an already-canceled ctx returned err = %v, want context.Canceled", err)
	}
}
