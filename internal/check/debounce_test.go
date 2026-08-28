package check

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/overlay"
)

// TestEngine_Invalidate_Debounces covers (d): a burst of Invalidate calls
// for the same directory collapses into a single recheck.
func TestEngine_Invalidate_Debounces(t *testing.T) {
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
}

// gatingReader wraps a FileReader so its very first ReadFile call signals
// started and then blocks until release is closed. Later calls pass
// through untouched. Used to hold a recheck job "in flight" long enough to
// observe cancellation.
type gatingReader struct {
	overlay.FileReader
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (g *gatingReader) ReadFile(path string) ([]byte, error) {
	g.once.Do(func() {
		close(g.started)
		<-g.release
	})
	return g.FileReader.ReadFile(path)
}

// TestEngine_Invalidate_CancelsInFlightRecheck covers (e): a recheck still
// running when a later debounce fires is canceled before the next one
// starts, so only the superseding recheck's result is published.
func TestEngine_Invalidate_CancelsInFlightRecheck(t *testing.T) {
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
}
