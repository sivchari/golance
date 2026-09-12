package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeTruncatedDB creates a real database at path with one entry, then
// truncates it to a length no valid bbolt meta page can occupy — a stand-in
// for a write torn by, e.g., a full disk or an external truncation.
func writeTruncatedDB(t *testing.T, path string) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() (seed): %v", err)
	}
	if err := db.PutUnit(&UnitEntry{PkgHash: 1, Pointer: UnitPointer{BlobKey: 42}}); err != nil {
		t.Fatalf("PutUnit (seed): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close (seed): %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for truncate: %v", err)
	}
	if err := f.Truncate(100); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close after truncate: %v", err)
	}
}

// writeGarbageDB writes bytes that were never a bbolt database at all.
func writeGarbageDB(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("not a bbolt database at all, just garbage 1234567890"), 0o600); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}
}

// assertSelfHealedAndUsable opens path (expected to self-heal via Open's
// corrupt-file discard-and-recreate) and verifies the result is a genuinely
// fresh, writable database rather than a handle left in some half-broken
// state.
func assertSelfHealedAndUsable(t *testing.T, path string) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() on a corrupt file = %v, want it to self-heal by discarding and recreating", err)
	}
	defer func() { _ = db.Close() }()

	if !db.WasRecreated() {
		t.Error("WasRecreated() = false, want true for a discarded corrupt file")
	}

	// The recreated database must be genuinely usable: the old
	// (unrecoverable) content is gone, and new writes succeed.
	if _, err := db.GetUnit(context.Background(), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUnit after recreate = %v, want ErrNotFound (old content must not resurrect)", err)
	}
	if err := db.PutUnit(&UnitEntry{PkgHash: 7, Pointer: UnitPointer{BlobKey: 99}}); err != nil {
		t.Errorf("PutUnit after recreate: %v", err)
	}
	got, err := db.GetUnit(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetUnit after recreate: %v", err)
	}
	if got.BlobKey != 99 {
		t.Errorf("GetUnit after recreate: BlobKey = %d, want 99", got.BlobKey)
	}
}

// TestOpen_CorruptFileSelfHeals verifies that Open discards and recreates a
// database file bbolt cannot parse at all (both meta pages invalid — see
// isCorrupt), the same self-healing discardStale already provides for a
// stale schemaVersion. Before this fix, such a file — e.g. left behind by an
// external truncation, a torn write from a full disk, or a bad backup
// restore — failed Open forever: unlike ErrTimeout (another live session
// holding the lock), nothing about that failure mode ever resolves on its
// own.
func TestOpen_CorruptFileSelfHeals(t *testing.T) {
	t.Run("truncated valid database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.db")
		writeTruncatedDB(t, path)
		assertSelfHealedAndUsable(t, path)
	})
	t.Run("never-a-database garbage bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.db")
		writeGarbageDB(t, path)
		assertSelfHealedAndUsable(t, path)
	})
}

// TestOpen_LockedFileIsNeverTreatedAsCorrupt guards the dangerous direction
// of the corrupt-file fix above: a database currently held open (write-locked)
// by another live session must be reported via IsLocked exactly as before,
// never discarded. isCorrupt and IsLocked classify mutually exclusive bbolt
// errors, but this pins that at the Open call itself, since deleting a live
// session's database out from under it would be a correctness disaster the
// unit-level isCorrupt/IsLocked tests alone would not catch.
func TestOpen_LockedFileIsNeverTreatedAsCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatalf("Open() (holder): %v", err)
	}
	defer func() { _ = holder.Close() }()

	// Pays Open's full openTimeout (5s): Open itself has no timeout override
	// for a test to inject, and this must exercise the exact same code path
	// the corrupt-file self-heal above was added to (Open's bbolt.Open
	// failure branch), not OpenReadOnlyTimeout's separate, already-tested
	// one.
	if _, err := Open(path); err == nil {
		t.Fatal("Open() on a locked file = nil error, want IsLocked")
	} else if !IsLocked(err) {
		t.Errorf("Open() on a locked file = %v, want IsLocked(err) == true", err)
	}

	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("locked database file removed by a concurrent Open: %v", statErr)
	}
}

// TestCASGC_ReclaimsOrphanedPutTempFile verifies that a "tmp-*.blob" file
// left behind in a shard directory by a Put call interrupted between
// os.CreateTemp and os.Rename (a crash, e.g. kill -9, mid-write — see
// (*CAS).Put's own doc) is eventually reclaimed by GC, once it is older
// than GraceWindow, exactly like an ordinary unreferenced blob — rather than
// silently surviving forever because blobKeyFromFilename cannot parse a key
// out of it.
func TestCASGC_ReclaimsOrphanedPutTempFile(t *testing.T) {
	dir := t.TempDir()
	cas, err := OpenCAS(dir)
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}

	// A real blob, so the shard directory (and its sibling entries) exist.
	if err := cas.Put(1, []byte("blob")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	shardPath := filepath.Join(dir, fmt.Sprintf("%016x", uint64(1))[:2])
	tmpPath := filepath.Join(shardPath, "tmp-crashed12345.blob")
	if err := os.WriteFile(tmpPath, []byte("partial write"), 0o600); err != nil {
		t.Fatalf("write orphaned temp file: %v", err)
	}
	old := time.Now().Add(-GraceWindow - time.Hour)
	if err := os.Chtimes(tmpPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	stats, err := cas.GC(time.Now(), map[uint64]struct{}{1: {}})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("orphaned temp file still present after GC: statErr = %v, want IsNotExist", statErr)
	}
	if !cas.Has(1) {
		t.Error("real blob 1 was swept, want kept (it is marked)")
	}
	if stats.SweptCount != 1 {
		t.Errorf("SweptCount = %d, want 1 (the orphaned temp file alone)", stats.SweptCount)
	}
}

// TestCASGC_YoungOrphanedPutTempFileSurvives verifies a "tmp-*.blob" file
// younger than GraceWindow is left alone: it may belong to a Put that is
// still genuinely in flight (CreateTemp has run, Rename has not yet), the
// same race GraceWindow's own doc describes for an ordinary just-written
// blob.
func TestCASGC_YoungOrphanedPutTempFileSurvives(t *testing.T) {
	dir := t.TempDir()
	cas, err := OpenCAS(dir)
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	if err := cas.Put(1, []byte("blob")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	shardPath := filepath.Join(dir, fmt.Sprintf("%016x", uint64(1))[:2])
	tmpPath := filepath.Join(shardPath, "tmp-inflight99999.blob")
	if err := os.WriteFile(tmpPath, []byte("still being written"), 0o600); err != nil {
		t.Fatalf("write in-flight temp file: %v", err)
	}

	if _, err := cas.GC(time.Now(), map[uint64]struct{}{1: {}}); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if _, statErr := os.Stat(tmpPath); statErr != nil {
		t.Errorf("young in-flight temp file removed by GC: statErr = %v, want it to still exist", statErr)
	}
}

// TestOpen_ConcurrentColdStartsAgainstSameNewPath simulates two golance
// sessions cold-starting an indexer build against the same not-yet-existing
// shared index database at once (see internal/server's buildIndexLocked
// doc): two real goroutines race Open against a brand new path with no
// synchronization beyond a start barrier. Exactly one must win outright;
// the other must either succeed once the winner closes (this test's own
// timing) or fail with IsLocked — never corrupt the file, never hang past
// openTimeout, and never panic under -race.
func TestOpen_ConcurrentColdStartsAgainstSameNewPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	const n = 8
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			db, err := Open(path)
			if err != nil {
				results <- err
				return
			}
			err = db.PutUnit(&UnitEntry{PkgHash: 1, Pointer: UnitPointer{BlobKey: 1}})
			results <- errors.Join(err, db.Close())
		}()
	}
	start.Done()
	wg.Wait()
	close(results)

	for err := range results {
		if err != nil && !IsLocked(err) {
			t.Errorf("concurrent cold-start Open/PutUnit/Close = %v, want nil or IsLocked", err)
		}
	}

	// The file must end up as a genuinely valid, openable database — not
	// corrupted by two writers racing bbolt's own file lock.
	final, err := Open(path)
	if err != nil {
		t.Fatalf("Open() after concurrent cold starts: %v", err)
	}
	defer func() { _ = final.Close() }()
	if _, err := final.GetUnit(context.Background(), 1); err != nil {
		t.Errorf("GetUnit after concurrent cold starts: %v", err)
	}
}
