package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/store"
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

// TestReindex_FatalPersistFailureAbortsClosureWalk verifies that a db
// persist failure during the reverse-dependency closure walk is surfaced
// as fatal and stops the walk, instead of being swallowed as an ordinary
// per-package error and re-attempted on every remaining package. leaf
// itself is left unchanged, so its own reindexOne call is a genuine skip
// (no write attempted); mid and top are both independently edited so each
// would need a real persist if reached. The db is reopened read-only after
// the initial Build, so mid's persist — the first closure hop — fails
// deterministically; top must never even be attempted.
func TestReindex_FatalPersistFailureAbortsClosureWalk(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	cas := openTestCAS(t)
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "index.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	writeFile(t, dir, "mid/mid.go", `// Package mid depends on leaf.
package mid

import (
	"strings"

	"example.com/idxmod/leaf"
)

// Shout returns an uppercase, exclaimed greeting for name.
func Shout(name string) string {
	g := leaf.Hello(name)
	return strings.ToUpper(g.Message) + "!"
}
`)
	writeFile(t, dir, "top/top.go", `// Package top depends on both leaf and mid.
package top

import (
	"example.com/idxmod/leaf"
	"example.com/idxmod/mid"
)

// Run exercises both leaf and mid, twice.
func Run(name string) string {
	direct := leaf.Hello(name).Message
	shouted := mid.Shout(name)
	return direct + shouted + shouted
}
`)

	ro, err := store.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("store.OpenReadOnly: %v", err)
	}
	defer func() {
		if err := ro.Close(); err != nil {
			t.Errorf("ro.Close: %v", err)
		}
	}()

	stats, err := Reindex(ctx, snap, ro, cas, pkgLeaf, readFileDisk, Options{})
	if err == nil {
		t.Fatal("Reindex returned nil error; want a fatal persist failure")
	}
	if !strings.Contains(err.Error(), "mid") {
		t.Errorf("Reindex error = %v, want it to name the mid package whose persist failed", err)
	}
	if stats.Errors != 1 {
		t.Errorf("stats.Errors = %d, want 1 (only mid's persist failure; top must never be attempted)", stats.Errors)
	}
	if stats.Processed != 1 {
		t.Errorf("stats.Processed = %d, want 1 (mid only)", stats.Processed)
	}
}
