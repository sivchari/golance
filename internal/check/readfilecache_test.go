package check

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sivchari/golance/internal/overlay"
)

// countingReader wraps *overlay.Overlay, counting calls to ReadFile per
// path. Embedding *overlay.Overlay (rather than reimplementing
// overlay.FileReader from scratch) means it still satisfies dirLister and
// openChecker via promoted methods, exactly like the real reader
// Engine.readFile type-asserts for in production.
type countingReader struct {
	*overlay.Overlay

	mu    sync.Mutex
	reads map[string]int
}

func newCountingReader() *countingReader {
	return &countingReader{Overlay: overlay.New(), reads: make(map[string]int)}
}

func (r *countingReader) ReadFile(path string) ([]byte, error) {
	r.mu.Lock()
	r.reads[path]++
	r.mu.Unlock()
	return r.Overlay.ReadFile(path)
}

func (r *countingReader) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.reads {
		n += c
	}
	return n
}

// TestEngine_Get_RepeatedGetOnUnchangedPackageAvoidsDiskRereads covers a
// second Get for the same package, with no edits in between, not re-reading
// any file from disk to recompute resolveFiles' package-clause filter and
// contentHash's staleness check — both run unconditionally on every Get,
// including a cache hit, so without memoization by disk (mtime, size) (see
// Engine.readFile) every repeated Get on an unchanged package pays a full
// disk read of every file in it just to confirm nothing changed. This is
// what made hover ~50-60ms on a large real-world package even though the
// type check itself was already cached and free.
func TestEngine_Get_RepeatedGetOnUnchangedPackageAvoidsDiskRereads(t *testing.T) {
	reader := newCountingReader()
	e, root := newTestEngine(t, reader, Options{})
	path := filepath.Join(root, "basic", "basic.go")
	ctx := context.Background()

	cp1, err := e.Get(ctx, path)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	firstTotal := reader.total()
	if firstTotal == 0 {
		t.Fatal("first Get performed no reads at all — test setup is broken")
	}

	cp2, err := e.Get(ctx, path)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if cp2 != cp1 {
		t.Fatal("second Get returned a different *CheckedPackage for unchanged content, want the cached one")
	}

	if got := reader.total(); got != firstTotal {
		t.Errorf("second Get performed %d additional disk read(s) (total %d -> %d) for a package unchanged since the first Get, want 0 additional reads",
			got-firstTotal, firstTotal, got)
	}
}
