package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const midSrcPath = "testdata/module/mid/mid.go"

// overlayReader returns a FileReader that serves content for path (an
// editor overlay) and falls back to disk for everything else.
func overlayReader(t *testing.T, path string, content []byte) FileReader {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	return func(p string) ([]byte, error) {
		if p == abs {
			return content, nil
		}
		return os.ReadFile(filepath.Clean(p))
	}
}

// TestReindex_BodyOnlyEditDoesNotPropagate verifies that editing a
// package's function body without changing its exported API only
// reprocesses that package, not its reverse-dependency closure.
func TestReindex_BodyOnlyEditDoesNotPropagate(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}

	edited := []byte(`// Package mid depends on leaf.
package mid

import (
	"strings"

	"example.com/idxmod/leaf"
)

// Shout returns an uppercase greeting for name.
func Shout(name string) string {
	greeting := leaf.Hello(name)
	return strings.ToUpper(greeting.Message)
}
`)
	reader := overlayReader(t, midSrcPath, edited)

	stats, err := Reindex(ctx, snap, db, cas, pkgMid, reader, Options{})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed = %d, want 1 (mid only, no propagation to top)", stats.Processed)
	}
}

// TestReindex_SignatureChangePropagates verifies that a signature-changing
// edit propagates the recheck to the reverse-dependency closure.
func TestReindex_SignatureChangePropagates(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}

	edited := []byte(`// Package mid depends on leaf.
package mid

import (
	"strings"

	"example.com/idxmod/leaf"
)

// Shout returns an uppercase greeting for name, repeated n times.
func Shout(name string, n int) string {
	g := leaf.Hello(name)
	out := strings.ToUpper(g.Message)
	for i := 1; i < n; i++ {
		out += out
	}
	return out
}
`)
	reader := overlayReader(t, midSrcPath, edited)

	stats, err := Reindex(ctx, snap, db, cas, pkgMid, reader, Options{})
	if err != nil {
		t.Logf("Reindex returned error (expected: top.go still calls the old 1-arg Shout): %v", err)
	}
	if stats.Processed != 2 {
		t.Errorf("Processed = %d, want 2 (mid and top, since mid's export data changed)", stats.Processed)
	}
}
