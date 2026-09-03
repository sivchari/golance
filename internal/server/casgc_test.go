package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/store"
)

// blobPathForTest reconstructs the on-disk path store.CAS uses for key,
// mirroring the documented "hex[:2]/hex[2:].blob" sharding layout (see
// store.CAS's blobPath doc and (*store.CAS).GC's blobKeyFromFilename doc).
// internal/store has no reason to export this itself — production code
// only ever needs a blob's key, never its path — but these tests need to
// backdate a specific blob's mtime past store.GraceWindow to exercise a
// real sweep end-to-end through RunCASGC.
func blobPathForTest(casDir string, key uint64) string {
	hex := fmt.Sprintf("%016x", key)
	return filepath.Join(casDir, hex[:2], hex[2:]+".blob")
}

// backdateBlob rewrites path's mtime to just past store.GraceWindow ago, so
// an unreferenced blob at that path is eligible for a GC sweep.
func backdateBlob(t *testing.T, casDir string, key uint64) {
	t.Helper()
	old := time.Now().Add(-store.GraceWindow - time.Hour)
	if err := os.Chtimes(blobPathForTest(casDir, key), old, old); err != nil {
		t.Fatalf("Chtimes(%d): %v", key, err)
	}
}

// putUnitDB opens (creating) a fresh index database at path, records
// casPath as its owning CAS directory, and stores one UnitPointer
// referencing blobKey — the minimal shape RunCASGC's mark-set collection
// needs from a "database sharing this CAS directory" fixture.
func putUnitDB(t *testing.T, path, casPath string, blobKey uint64) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", path, err)
	}
	if err := db.PutCASDir(casPath); err != nil {
		t.Fatalf("PutCASDir: %v", err)
	}
	if err := db.PutUnit(&store.UnitEntry{PkgHash: blobKey, Pointer: store.UnitPointer{BlobKey: blobKey}}); err != nil {
		t.Fatalf("PutUnit: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
}

// TestRunCASGC_UnionAcrossDatabasesProtectsReferencedBlob is the multi-DB
// mark union test: a blob referenced only by an OTHER index database (not
// ownDB) sharing the same CAS directory must survive exactly as one
// referenced by ownDB itself does, while a genuinely unreferenced blob is
// swept.
func TestRunCASGC_UnionAcrossDatabasesProtectsReferencedBlob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	casPath := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	for _, key := range []uint64{1, 2, 3} {
		if err := cas.Put(key, []byte("blob")); err != nil {
			t.Fatalf("Put(%d): %v", key, err)
		}
		backdateBlob(t, casPath, key)
	}

	cacheDir := filepath.Join(cacheBaseDir(), "golance")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}

	ownPath := filepath.Join(cacheDir, "index-own.db")
	own, err := store.Open(ownPath)
	if err != nil {
		t.Fatalf("store.Open(own): %v", err)
	}
	t.Cleanup(func() { _ = own.Close() })
	if err := own.PutUnit(&store.UnitEntry{PkgHash: 1, Pointer: store.UnitPointer{BlobKey: 1}}); err != nil {
		t.Fatalf("PutUnit(own): %v", err)
	}

	// A different index database (e.g. another worktree's own private/root
	// index) sharing the same CAS directory, referencing blob 2. Its
	// contribution is only visible to RunCASGC via the CASDir meta value
	// (see (*store.DB).CASDir's doc).
	putUnitDB(t, filepath.Join(cacheDir, "index-other.db"), casPath, 2)

	// blob 3 is referenced by nothing at all.

	stats, ran := RunCASGC(t.Logf, casPath, ownPath, own, true)
	if !ran {
		t.Fatal("RunCASGC(force=true) ran = false, want true")
	}
	if !cas.Has(1) {
		t.Error("blob 1 (marked by ownDB) was swept, want kept")
	}
	if !cas.Has(2) {
		t.Error("blob 2 (marked only by another database sharing the CAS dir) was swept, want kept — multi-DB mark union must include it")
	}
	if cas.Has(3) {
		t.Error("blob 3 (unreferenced by anything) survived, want swept")
	}
	if stats.SweptCount != 1 {
		t.Errorf("SweptCount = %d, want 1", stats.SweptCount)
	}
	if stats.KeptCount != 2 {
		t.Errorf("KeptCount = %d, want 2", stats.KeptCount)
	}
}

// TestRunCASGC_MismatchedOrMissingCASDirIgnored verifies collectOtherCASMarks
// only merges marks from a database whose own recorded CASDir matches the
// CAS directory being swept: a database for an unrelated repository, and
// one that predates the CASDir meta field entirely (CASDir returns
// ErrNotFound), must both be excluded even though their files sit in the
// same shared cache directory.
func TestRunCASGC_MismatchedOrMissingCASDirIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	casPath := filepath.Join(t.TempDir(), "cas")
	otherCASPath := filepath.Join(t.TempDir(), "other-cas")

	cacheDir := filepath.Join(cacheBaseDir(), "golance")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}

	// Belongs to a different CAS directory entirely.
	putUnitDB(t, filepath.Join(cacheDir, "index-unrelated.db"), otherCASPath, 999)

	// Predates the CASDir meta field: never had PutCASDir called on it.
	predates, err := store.Open(filepath.Join(cacheDir, "index-legacy.db"))
	if err != nil {
		t.Fatalf("store.Open(legacy): %v", err)
	}
	if err := predates.PutUnit(&store.UnitEntry{PkgHash: 1, Pointer: store.UnitPointer{BlobKey: 888}}); err != nil {
		t.Fatalf("PutUnit(legacy): %v", err)
	}
	if err := predates.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	marks := map[uint64]struct{}{}
	collectOtherCASMarks(casPath, "", marks)

	if len(marks) != 0 {
		t.Errorf("collectOtherCASMarks() marks = %v, want empty (neither database's CASDir matches casPath)", marks)
	}
}

// TestRunCASGC_LockedOtherDatabaseIsSkipped documents the safety tradeoff
// (*store.CAS).GC's doc describes: a database currently held open by
// another live writer cannot be read within otherDBOpenTimeout, so its
// references are excluded from this round's mark set — a blob it alone
// references, old enough to be past GraceWindow, is swept despite still
// being that (unreachable-this-round) database's current target. This is
// the documented tradeoff, not a bug: CAS.Get's self-healing miss path
// means the worst outcome is a recompute, never data loss.
func TestRunCASGC_LockedOtherDatabaseIsSkipped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	casPath := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if err := cas.Put(1, []byte("blob")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	backdateBlob(t, casPath, 1)

	cacheDir := filepath.Join(cacheBaseDir(), "golance")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}

	ownPath := filepath.Join(cacheDir, "index-own.db")
	own, err := store.Open(ownPath)
	if err != nil {
		t.Fatalf("store.Open(own): %v", err)
	}
	t.Cleanup(func() { _ = own.Close() })
	// ownDB references nothing; only the locked database below references
	// blob 1.

	lockedPath := filepath.Join(cacheDir, "index-locked.db")
	locked, err := store.Open(lockedPath)
	if err != nil {
		t.Fatalf("store.Open(locked): %v", err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	if err := locked.PutCASDir(casPath); err != nil {
		t.Fatalf("PutCASDir(locked): %v", err)
	}
	if err := locked.PutUnit(&store.UnitEntry{PkgHash: 1, Pointer: store.UnitPointer{BlobKey: 1}}); err != nil {
		t.Fatalf("PutUnit(locked): %v", err)
	}
	// locked is deliberately left open (holding bbolt's exclusive lock, see
	// store.DB's doc) rather than closed here, simulating a live session
	// still using it.

	stats, ran := RunCASGC(t.Logf, casPath, ownPath, own, true)
	if !ran {
		t.Fatal("RunCASGC(force=true) ran = false, want true")
	}
	if cas.Has(1) {
		t.Error("blob 1, referenced only by a currently-locked database, survived GC; want swept (documented tradeoff — see this test's doc)")
	}
	if stats.SweptCount != 1 {
		t.Errorf("SweptCount = %d, want 1", stats.SweptCount)
	}
}

// TestRunCASGC_ForceBypassesThrottle verifies force plumbs through to
// (*store.CAS).GC (always runs) versus MaybeGC (throttled to once per
// store.GCInterval): a second force=false call right after a first must be
// a no-op (ran=false), while a subsequent force=true call still runs.
func TestRunCASGC_ForceBypassesThrottle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	casPath := filepath.Join(t.TempDir(), "cas")
	if _, err := store.OpenCAS(casPath); err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}

	cacheDir := filepath.Join(cacheBaseDir(), "golance")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	ownPath := filepath.Join(cacheDir, "index-own.db")
	own, err := store.Open(ownPath)
	if err != nil {
		t.Fatalf("store.Open(own): %v", err)
	}
	t.Cleanup(func() { _ = own.Close() })

	if _, ran := RunCASGC(t.Logf, casPath, ownPath, own, false); !ran {
		t.Fatal("first RunCASGC(force=false) ran = false, want true (no stamp yet)")
	}
	if _, ran := RunCASGC(t.Logf, casPath, ownPath, own, false); ran {
		t.Error("second RunCASGC(force=false) ran = true within GCInterval, want false")
	}
	if _, ran := RunCASGC(t.Logf, casPath, ownPath, own, true); !ran {
		t.Error("RunCASGC(force=true) ran = false, want true (bypasses the interval throttle)")
	}
}

// TestServer_RunStartupCASGC_NoIndexIsNoop verifies runStartupCASGC does
// nothing (and in particular never creates a CAS directory) when no index
// is open yet — the tryWarmOpen/buildIndex-failed case.
func TestServer_RunStartupCASGC_NoIndexIsNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root

	s.runStartupCASGC(root)

	if _, err := os.Stat(casDir(root)); !os.IsNotExist(err) {
		t.Errorf("runStartupCASGC with no index created %s, want it to remain absent", casDir(root))
	}
}

// TestServer_RunStartupCASGC_WithIndexRunsGC verifies runStartupCASGC, once
// an index is installed, actually walks the CAS directory (observable via
// the GC stamp file MaybeGC writes) rather than silently doing nothing.
func TestServer_RunStartupCASGC_WithIndexRunsGC(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newWorkspaceOnlyServer(t)
	root := s.workspace().root
	snap := s.workspace().snap

	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, dbPath, cas)

	idx, ok := s.tryWarmOpen(root)
	if !ok {
		t.Fatal("tryWarmOpen() = not ok, want ok")
	}
	s.idx.Store(idx)
	t.Cleanup(func() { _ = idx.db.Close() })

	s.runStartupCASGC(root)

	// A GC pass that actually ran leaves its stamp file behind (see
	// store.CAS.MaybeGC's doc) — the observable side effect proving this
	// wiring reached the real directory walk, not just a no-op early
	// return.
	if _, err := os.Stat(filepath.Join(casDir(root), ".gc-stamp")); err != nil {
		t.Errorf("runStartupCASGC did not leave a GC stamp file behind: %v", err)
	}
}
