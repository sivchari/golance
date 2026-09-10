package index

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// panicReader returns a FileReader that panics when asked to read path,
// standing in for a type-checker edge case processUnit does not yet
// handle, and falls back to disk for everything else.
func panicReader(t *testing.T, path string) FileReader {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	return func(p string) ([]byte, error) {
		if p == abs {
			panic("deliberate processUnit panic")
		}
		return os.ReadFile(filepath.Clean(p))
	}
}

// blockingOverlayReader returns a FileReader that serves content for path,
// closing started (once) and then waiting on release before returning it
// each time path is read — letting a test force a specific interleaving
// between this reader's own Reindex call and a concurrent one. It falls
// back to disk immediately for everything else.
func blockingOverlayReader(t *testing.T, path string, content []byte, started chan<- struct{}, release <-chan struct{}) FileReader {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	var once sync.Once
	return func(p string) ([]byte, error) {
		if p == abs {
			once.Do(func() { close(started) })
			<-release
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

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
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

	stats, err := Reindex(ctx, snap, db, cas, pkgMid, reader, &Options{})
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

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
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

	stats, err := Reindex(ctx, snap, db, cas, pkgMid, reader, &Options{})
	if err != nil {
		t.Logf("Reindex returned error (expected: top.go still calls the old 1-arg Shout): %v", err)
	}
	if stats.Processed != 2 {
		t.Errorf("Processed = %d, want 2 (mid and top, since mid's export data changed)", stats.Processed)
	}
}

// TestReindex_Stats_ChangedTracksActuallyReprocessedHops verifies
// Stats.Changed reflects exactly the hops Reindex actually reprocessed
// (skipped == false), not the whole reverse-dependency closure it walked:
// a body-only edit to mid (imported by top) must list mid alone, while a
// signature-changing edit must list both mid and top.
func TestReindex_Stats_ChangedTracksActuallyReprocessedHops(t *testing.T) {
	tests := []struct {
		name   string
		edited []byte
		want   []string
	}{
		{
			name: "body only edit",
			edited: []byte(`// Package mid depends on leaf.
package mid

import (
	"strings"

	"example.com/idxmod/leaf"
)

// Shout returns an uppercase greeting for name.
func Shout(name string) string {
	greeting := leaf.Hello(name)
	return strings.ToUpper(greeting.Message + "!")
}
`),
			want: []string{pkgMid},
		},
		{
			name: "signature changing edit",
			edited: []byte(`// Package mid depends on leaf.
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
`),
			want: []string{pkgMid, pkgTop},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := loadTestSnapshot(t)
			db := openTestDB(t)
			cas := openTestCAS(t)
			ctx := context.Background()

			if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
				t.Fatalf("initial Build: %v", err)
			}

			reader := overlayReader(t, midSrcPath, tt.edited)
			stats, err := Reindex(ctx, snap, db, cas, pkgMid, reader, &Options{})
			if err != nil {
				t.Logf("Reindex returned error (may be expected: top.go's call site can be stale): %v", err)
			}
			if !slices.Equal(stats.Changed, tt.want) {
				t.Errorf("stats.Changed = %v, want %v", stats.Changed, tt.want)
			}
		})
	}
}

// TestReindex_TestFileEditProcessedButExportUnchanged verifies three things
// about editing an in-package _test.go file:
//   - it is processed and actually type-checked (its content, and so
//     [store.UnitPointer].ContentHash/BlobKey, changed);
//   - it does not propagate to the package's reverse-dependency closure,
//     since a test file never contributes to a package's exported API (see
//     checkOnePackage's doc); and
//   - a symbol newly declared only in the edited test file is indexed
//     afterward, confirming the reindex actually re-covers test files, not
//     just re-hashes them.
func TestReindex_TestFileEditProcessedButExportUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/testreindex\n\ngo 1.23\n")
	writeFile(t, dir, "def/def.go", `package def

// V returns 1.
func V() int { return 1 }
`)
	const testSrc1 = `package def

import "testing"

func TestV(t *testing.T) {
	if V() != 1 {
		t.Fatal("bad")
	}
}
`
	testPath := filepath.Join(dir, "def", "def_test.go")
	writeFile(t, dir, "def/def_test.go", testSrc1)
	writeFile(t, dir, "use/use.go", `package use

import "example.com/testreindex/def"

// Call calls def.V.
func Call() int { return def.V() }
`)

	const pkgDef = "example.com/testreindex/def"

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}
	before, err := db.GetUnit(ctx, store.Hash(pkgDef))
	if err != nil {
		t.Fatalf("GetUnit(def): %v", err)
	}

	const testSrc2 = testSrc1 + `
// helperV is declared only in this in-package test file.
func helperV() int { return V() }
`
	reader := overlayReader(t, testPath, []byte(testSrc2))

	stats, err := Reindex(ctx, snap, db, cas, pkgDef, reader, &Options{})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed = %d, want 1 (def only; a test-file-only edit must not force use to be revisited)", stats.Processed)
	}
	if stats.TypeChecked != 1 {
		t.Errorf("TypeChecked = %d, want 1 (content changed, so a real type-check, not a skip)", stats.TypeChecked)
	}

	after, err := db.GetUnit(ctx, store.Hash(pkgDef))
	if err != nil {
		t.Fatalf("GetUnit(def) after Reindex: %v", err)
	}
	if after.ExportHash != before.ExportHash {
		t.Errorf("ExportHash changed after a test-file-only edit (before=%d after=%d); export data must be derived from non-test files alone", before.ExportHash, after.ExportHash)
	}
	if after.BlobKey == before.BlobKey {
		t.Error("BlobKey unchanged after a test-file-only content edit, want it to change (own content hash must cover in-package test files)")
	}

	findSymbolByName(t, db, cas, pkgDef, "helperV")
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
	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
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

	stats, err := Reindex(ctx, snap, ro, cas, pkgLeaf, readFileDisk, &Options{})
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

// TestReindex_PanicDuringProcessingIsRecordedAsPerPackageError verifies
// that a panic while resolving mid's own unit (e.g. a type-checker edge
// case facts extraction does not yet handle) is recovered by
// processUnitRecovered and reported through Reindex as mid's own
// per-package error, instead of taking down the caller — the didSave/
// self-heal path this exercises used to call processUnit directly, with no
// recovery, so this same panic used to crash the whole server.
func TestReindex_PanicDuringProcessingIsRecordedAsPerPackageError(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	reader := panicReader(t, midSrcPath)

	stats, err := Reindex(ctx, snap, db, cas, pkgMid, reader, &Options{})
	if err == nil {
		t.Fatal("Reindex returned nil error, want the recovered panic reported as mid's own error")
	}
	if !strings.Contains(err.Error(), pkgMid) {
		t.Errorf("Reindex error = %v, want it to name %s", err, pkgMid)
	}
	if stats.Errors != 1 {
		t.Errorf("stats.Errors = %d, want 1", stats.Errors)
	}
	if !strings.Contains(logBuf.String(), "deliberate processUnit panic") {
		t.Errorf("log output = %q, want it to contain the panic value", logBuf.String())
	}
}

// TestReindex_ConcurrentReindexDoesNotLetOlderClobberNewer verifies the
// invariant genTable exists to uphold: two Reindex calls racing on the same
// package, with no ordering guarantee between them, must never let a call
// started earlier but still running overwrite a faster call's write that
// already committed after it started — otherwise the index would end up
// describing content no longer on disk, with nothing to detect it (see
// H7). The older call's reader is held blocked on its own read of mid.go
// until the newer call has already persisted, forcing the exact
// interleaving that used to clobber the newer write.
func TestReindex_ConcurrentReindexDoesNotLetOlderClobberNewer(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}

	older := []byte(`// Package mid depends on leaf.
package mid

import (
	"strings"

	"example.com/idxmod/leaf"
)

// Shout returns an uppercase greeting for name (older save).
func Shout(name string) string {
	greeting := leaf.Hello(name)
	return strings.ToUpper(greeting.Message) + "?"
}
`)
	newer := []byte(`// Package mid depends on leaf.
package mid

import (
	"strings"

	"example.com/idxmod/leaf"
)

// Shout returns an uppercase greeting for name (newer save).
func Shout(name string) string {
	greeting := leaf.Hello(name)
	return strings.ToUpper(greeting.Message) + "!"
}
`)

	started := make(chan struct{})
	release := make(chan struct{})
	olderReader := blockingOverlayReader(t, midSrcPath, older, started, release)

	olderDone := make(chan error, 1)
	go func() {
		_, err := Reindex(ctx, snap, db, cas, pkgMid, olderReader, &Options{})
		olderDone <- err
	}()

	<-started // the older call has been assigned its generation and is now blocked reading mid.go.

	newerReader := overlayReader(t, midSrcPath, newer)
	if _, err := Reindex(ctx, snap, db, cas, pkgMid, newerReader, &Options{}); err != nil {
		t.Fatalf("newer Reindex: %v", err)
	}

	close(release)
	if err := <-olderDone; err != nil {
		t.Fatalf("older Reindex: %v", err)
	}

	got, err := db.GetUnit(ctx, store.Hash(pkgMid))
	if err != nil {
		t.Fatalf("GetUnit(mid): %v", err)
	}

	refDB := openTestDB(t)
	if _, err := Build(ctx, snap, refDB, cas, &Options{}); err != nil {
		t.Fatalf("reference Build: %v", err)
	}
	if _, err := Reindex(ctx, snap, refDB, cas, pkgMid, overlayReader(t, midSrcPath, newer), &Options{}); err != nil {
		t.Fatalf("reference Reindex: %v", err)
	}
	want, err := refDB.GetUnit(ctx, store.Hash(pkgMid))
	if err != nil {
		t.Fatalf("GetUnit(mid) reference: %v", err)
	}

	if got.ContentHash != want.ContentHash || got.BlobKey != want.BlobKey {
		t.Errorf("db holds the older (slower) call's write; want the newer (faster) call's: got ContentHash=%d BlobKey=%d, want ContentHash=%d BlobKey=%d", got.ContentHash, got.BlobKey, want.ContentHash, want.BlobKey)
	}
}

// TestReindex_ReprocessesTestOnlyImporter is H11's end-to-end regression
// test: consumer's production code has no import of dep at all — only
// consumer_test.go (an in-package test file) does. Reindexing dep after an
// export-changing edit must still walk consumer (via
// graph.Snapshot.ClosureUnits, which now folds Package.TestImports into its
// reverse-dependency edges) AND actually rebuild consumer's own combined
// key (via directDepImports, which now folds pkg.TestImports into what
// directDepExports resolves) — otherwise consumer would be visited but
// resolve to an unchanged key and be silently skipped, leaving its facts
// (in particular consumer_test.go's own reference to dep.V) stale
// indefinitely.
func TestReindex_ReprocessesTestOnlyImporter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/testonlyreindex\n\ngo 1.23\n")
	writeFile(t, dir, "dep/dep.go", `// Package dep is imported only by consumer's in-package test file.
package dep

// V returns 1.
func V() int { return 1 }
`)
	writeFile(t, dir, "consumer/consumer.go", `// Package consumer has no production import of dep.
package consumer

// C returns 1.
func C() int { return 1 }
`)
	const testSrc = `package consumer

import (
	"testing"

	"example.com/testonlyreindex/dep"
)

// TestC exercises consumer's in-package test variant, the only place this
// package ever references dep.
func TestC(t *testing.T) {
	if C() != 1 || dep.V() != 1 {
		t.Fatal("unexpected result")
	}
}
`
	writeFile(t, dir, "consumer/consumer_test.go", testSrc)

	const pkgDep = "example.com/testonlyreindex/dep"
	const pkgConsumer = "example.com/testonlyreindex/consumer"

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}
	before, err := db.GetUnit(ctx, store.Hash(pkgConsumer))
	if err != nil {
		t.Fatalf("GetUnit(consumer) before: %v", err)
	}
	depV := findSymbolByName(t, db, cas, pkgDep, "V")
	var referencedBefore bool
	viewFacts(t, db, cas, pkgConsumer, func(v *store.View) {
		for _, r := range v.RefsTo(depV) {
			if r.ToPkgHash() == store.Hash(pkgDep) {
				referencedBefore = true
			}
		}
	})
	if !referencedBefore {
		t.Fatal("fixture invalid: consumer's initial facts have no ref to dep.V")
	}

	// An export-changing edit to dep (a new exported symbol) that
	// consumer_test.go never references itself: proves the propagation is
	// driven by dep's export hash changing, not by consumer_test.go's own
	// content changing.
	edited := []byte(`// Package dep is imported only by consumer's in-package test file.
package dep

// V returns 1.
func V() int { return 1 }

// W is a new exported symbol, added to change dep's export data.
func W() int { return 2 }
`)
	reader := overlayReader(t, filepath.Join(dir, "dep", "dep.go"), edited)

	stats, err := Reindex(ctx, snap, db, cas, pkgDep, reader, &Options{})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if !slices.Contains(stats.Changed, pkgConsumer) {
		t.Errorf("Stats.Changed = %v, want it to contain %s (a test-only importer of dep)", stats.Changed, pkgConsumer)
	}

	after, err := db.GetUnit(ctx, store.Hash(pkgConsumer))
	if err != nil {
		t.Fatalf("GetUnit(consumer) after: %v", err)
	}
	if after.BlobKey == before.BlobKey {
		t.Error("consumer's BlobKey unchanged after dep's export data changed; consumer was not actually reprocessed")
	}
}
