package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// never treats a database as a member of casPath unless store.CASMembers
// says so: a database that recorded a different CAS directory entirely
// never even becomes a candidate (its own membership marker lives under
// its own CAS directory, not casPath's), and neither does one that
// predates the CASDir meta field and so never called PutCASDir at all —
// both are excluded from the mark set without counting toward the
// returned unreadable total, even though their files sit in the same
// shared cache directory as a genuine candidate would.
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
	unreadable := collectOtherCASMarks(t.Logf, casPath, "", marks)

	if len(marks) != 0 {
		t.Errorf("collectOtherCASMarks() marks = %v, want empty (neither database's CASDir matches casPath)", marks)
	}
	if unreadable != 0 {
		t.Errorf("collectOtherCASMarks() unreadable = %d, want 0 (both databases opened and read cleanly)", unreadable)
	}
}

// casGCTestCache points HOME at a fresh temp dir and returns the golance
// cache directory beneath it, creating it if necessary — the shared setup
// every RunCASGC test needs before writing index database fixtures into
// it.
func casGCTestCache(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cacheDir := filepath.Join(cacheBaseDir(), "golance")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	return cacheDir
}

// openOwnCASGCTestDB opens (creating) a fresh index database at
// cacheDir/index-own.db, registers its cleanup, and returns both its path
// and handle — the "this call's own database" argument every RunCASGC test
// needs.
func openOwnCASGCTestDB(t *testing.T, cacheDir string) (path string, db *store.DB) {
	t.Helper()
	path = filepath.Join(cacheDir, "index-own.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(own): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return path, db
}

// captureLogf returns a logf argument for RunCASGC/collectOtherCASMarks
// that appends each formatted line to logged, for a test asserting on
// exactly what got logged.
func captureLogf() (logf func(format string, args ...any), logged *[]string) {
	logged = new([]string)
	logf = func(format string, args ...any) { *logged = append(*logged, fmt.Sprintf(format, args...)) }
	return logf, logged
}

// loggedLineContains reports whether any line in logged contains both a
// and b.
func loggedLineContains(logged []string, a, b string) bool {
	for _, line := range logged {
		if strings.Contains(line, a) && strings.Contains(line, b) {
			return true
		}
	}
	return false
}

// makeCorruptCASGCTestDB creates an index database at cacheDir that has
// recorded casPath as its own CAS directory, then overwrites it with bytes
// bbolt cannot parse as a database at all — simulating a file a prior
// crash (e.g. a full disk) left unreadable — and returns its path.
func makeCorruptCASGCTestDB(t *testing.T, cacheDir, casPath string) string {
	t.Helper()
	path := filepath.Join(cacheDir, "index-corrupt.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(corrupt): %v", err)
	}
	if err := db.PutCASDir(casPath); err != nil {
		t.Fatalf("PutCASDir(corrupt): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close corrupt db: %v", err)
	}
	if err := os.WriteFile(path, []byte("not a bbolt database"), 0o600); err != nil {
		t.Fatalf("corrupt db file: %v", err)
	}
	return path
}

// openLockedCASGCTestDB opens an index database at cacheDir/index-locked.db
// that records casPath as its own CAS directory and references blob 1,
// deliberately left open through t.Cleanup rather than closed within the
// caller's body — simulating a live session still using it, so a later
// RunCASGC call against casPath finds it locked.
func openLockedCASGCTestDB(t *testing.T, cacheDir, casPath string) string {
	t.Helper()
	path := filepath.Join(cacheDir, "index-locked.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(locked): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PutCASDir(casPath); err != nil {
		t.Fatalf("PutCASDir(locked): %v", err)
	}
	if err := db.PutUnit(&store.UnitEntry{PkgHash: 1, Pointer: store.UnitPointer{BlobKey: 1}}); err != nil {
		t.Fatalf("PutUnit(locked): %v", err)
	}
	return path
}

// TestRunCASGC_CorruptOtherDatabaseDoesNotDeferTheSweep verifies that a
// candidate which recorded casPath as its own CAS directory but can no
// longer be opened as a database at all (as opposed to one merely locked
// by a live writer) does not defer the sweep: it can neither contribute
// marks nor protect anything reachable, and self-heals to an empty
// database the next time its owning session opens it.
func TestRunCASGC_CorruptOtherDatabaseDoesNotDeferTheSweep(t *testing.T) {
	casPath := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if err := cas.Put(1, []byte("blob")); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	backdateBlob(t, casPath, 1)

	cacheDir := casGCTestCache(t)
	ownPath, own := openOwnCASGCTestDB(t, cacheDir)
	// ownDB references nothing; blob 1 is referenced by nothing at all and
	// must be swept once the corrupt candidate below is correctly excluded
	// rather than deferring the whole round.
	corruptPath := makeCorruptCASGCTestDB(t, cacheDir, casPath)

	logf, logged := captureLogf()
	stats, ran := RunCASGC(logf, casPath, ownPath, own, true)
	if !ran {
		t.Fatalf("RunCASGC(force=true) ran = false, want true (a corrupt candidate must not defer the sweep); logged: %v", *logged)
	}
	if cas.Has(1) {
		t.Error("blob 1 (unreferenced by anything, with the corrupt candidate correctly excluded) survived, want swept")
	}
	if stats.SweptCount != 1 {
		t.Errorf("SweptCount = %d, want 1", stats.SweptCount)
	}
	if !loggedLineContains(*logged, corruptPath, "not a usable index database") {
		t.Errorf("logged lines %v do not explain excluding the corrupt candidate %s", *logged, corruptPath)
	}
}

// TestRunCASGC_LockedDatabaseInDifferentCASNeverBlocks verifies the H10
// fix: a database currently locked by a live writer, but belonging to a
// completely different CAS directory, must never be treated as a
// candidate for casPath's sweep at all — since it never recorded casPath
// as its own CAS directory (store.CASMembers(casPath) never names it) —
// so it can never defer casPath's reclaim the way an unrelated
// repository's database could under a machine-wide glob.
func TestRunCASGC_LockedDatabaseInDifferentCASNeverBlocks(t *testing.T) {
	casPath := filepath.Join(t.TempDir(), "cas")
	otherCASPath := filepath.Join(t.TempDir(), "other-cas")
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if err := cas.Put(1, []byte("blob")); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	backdateBlob(t, casPath, 1)

	cacheDir := casGCTestCache(t)
	ownPath, own := openOwnCASGCTestDB(t, cacheDir)

	unrelatedPath := filepath.Join(cacheDir, "index-unrelated.db")
	unrelated, err := store.Open(unrelatedPath)
	if err != nil {
		t.Fatalf("store.Open(unrelated): %v", err)
	}
	t.Cleanup(func() { _ = unrelated.Close() })
	if err := unrelated.PutCASDir(otherCASPath); err != nil {
		t.Fatalf("PutCASDir(unrelated): %v", err)
	}
	if err := unrelated.PutUnit(&store.UnitEntry{PkgHash: 1, Pointer: store.UnitPointer{BlobKey: 999}}); err != nil {
		t.Fatalf("PutUnit(unrelated): %v", err)
	}
	// Deliberately left open (holding bbolt's exclusive lock) rather than
	// closed, simulating a live session for a completely unrelated
	// repository that happens to share this machine's cache directory.

	stats, ran := RunCASGC(t.Logf, casPath, ownPath, own, true)
	if !ran {
		t.Fatal("RunCASGC(force=true) ran = false, want true (an unrelated repository's locked database must never block this CAS's sweep)")
	}
	if cas.Has(1) {
		t.Error("blob 1 (unreferenced within casPath) survived, want swept")
	}
	if stats.SweptCount != 1 {
		t.Errorf("SweptCount = %d, want 1", stats.SweptCount)
	}
}

// TestRunCASGC_LockedOtherDatabaseDefersTheWholeSweep verifies the
// completeness invariant RunCASGC's own doc describes: a database currently
// held open by another live writer cannot be read within otherDBOpenTimeout,
// so the mark set this round would build is an under-approximation of what
// is genuinely still referenced. Sweeping against it anyway would risk
// deleting a blob that (unreachable-this-round) database's own UnitPointer
// still names — unlike an ordinary CAS miss, nothing ever repairs that. So
// RunCASGC must skip the sweep entirely, leaving every blob (including one
// past GraceWindow with no reader at all) untouched, and log why.
func TestRunCASGC_LockedOtherDatabaseDefersTheWholeSweep(t *testing.T) {
	casPath := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	for _, key := range []uint64{1, 2} {
		if err := cas.Put(key, []byte("blob")); err != nil {
			t.Fatalf("Put(%d): %v", key, err)
		}
		backdateBlob(t, casPath, key)
	}

	cacheDir := casGCTestCache(t)
	ownPath, own := openOwnCASGCTestDB(t, cacheDir)
	// ownDB references nothing; blob 1 is referenced only by the locked
	// database below, and blob 2 is referenced by nothing at all — both
	// must survive, since an incomplete mark set must not sweep anything.
	lockedPath := openLockedCASGCTestDB(t, cacheDir, casPath)

	logf, logged := captureLogf()
	stats, ran := RunCASGC(logf, casPath, ownPath, own, true)
	if ran {
		t.Error("RunCASGC(force=true) ran = true with an unreadable candidate database, want false (deferred)")
	}
	if stats != (store.GCStats{}) {
		t.Errorf("RunCASGC stats = %+v, want zero value when deferred", stats)
	}
	if !cas.Has(1) {
		t.Error("blob 1 was swept despite an incomplete mark set, want kept")
	}
	if !cas.Has(2) {
		t.Error("blob 2 was swept despite an incomplete mark set, want kept")
	}
	// One line names the specific candidate that blocked reclaim and why
	// (see collectOtherCASMarks's doc), one summarizes the deferral (see
	// RunCASGC's doc) — together, enough to diagnose a stalled reclaim
	// without reading source.
	if len(*logged) != 2 {
		t.Fatalf("logged %d line(s), want exactly 2 (one per-candidate, one summary): %v", len(*logged), *logged)
	}
	if !strings.Contains((*logged)[0], lockedPath) || !strings.Contains((*logged)[0], "locked") {
		t.Errorf("logged line %q does not name the blocking candidate %s", (*logged)[0], lockedPath)
	}
	if !strings.Contains((*logged)[1], "deferred") {
		t.Errorf("logged line %q does not mention the deferral", (*logged)[1])
	}
}

// TestRunCASGC_UndecodableOwnRecordDefersTheSweep regression-tests M7: an
// undecodable "unit" record in ownDB itself (not merely another database
// this round could not open) must feed the same deferred-sweep signal a
// locked candidate does, rather than being silently skipped by
// CollectBlobKeys with no way for RunCASGC to know its own mark set is
// incomplete.
func TestRunCASGC_UndecodableOwnRecordDefersTheSweep(t *testing.T) {
	casPath := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if err := cas.Put(1, []byte("blob")); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	backdateBlob(t, casPath, 1)

	cacheDir := casGCTestCache(t)
	ownPath, own := openOwnCASGCTestDB(t, cacheDir)
	// blob 1 is referenced only by the undecodable record below; if
	// RunCASGC failed to defer, it would look unreferenced and be swept.
	if err := store.PutRawUnitRecord(own, 1, []byte{0xff, 0xff}); err != nil {
		t.Fatalf("PutRawUnitRecord: %v", err)
	}

	logf, logged := captureLogf()
	stats, ran := RunCASGC(logf, casPath, ownPath, own, true)
	if ran {
		t.Error("RunCASGC(force=true) ran = true with an undecodable record in ownDB, want false (deferred)")
	}
	if stats != (store.GCStats{}) {
		t.Errorf("RunCASGC stats = %+v, want zero value when deferred", stats)
	}
	if !cas.Has(1) {
		t.Error("blob 1 was swept despite an undecodable own-DB record making the mark set incomplete, want kept")
	}
	if !loggedLineContains(*logged, "undecodable", "own") {
		t.Errorf("logged lines %v do not explain the own-DB decode failure", *logged)
	}
}

// TestRunCASGC_UndecodableOtherRecordDefersTheSweep is
// TestRunCASGC_UndecodableOwnRecordDefersTheSweep's counterpart for a
// database found via collectOtherCASMarks rather than ownDB: an
// undecodable record there must count toward the returned unreadable total
// exactly like a locked candidate does (see collectOtherCASMarks's own
// doc), rather than being silently swallowed by the discarded `_ =
// db.CollectBlobKeys(marks)` this replaces.
func TestRunCASGC_UndecodableOtherRecordDefersTheSweep(t *testing.T) {
	casPath := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if err := cas.Put(1, []byte("blob")); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	backdateBlob(t, casPath, 1)

	cacheDir := casGCTestCache(t)
	ownPath, own := openOwnCASGCTestDB(t, cacheDir)

	otherPath := filepath.Join(cacheDir, "index-other.db")
	other, err := store.Open(otherPath)
	if err != nil {
		t.Fatalf("store.Open(other): %v", err)
	}
	if err := other.PutCASDir(casPath); err != nil {
		t.Fatalf("PutCASDir(other): %v", err)
	}
	// blob 1 is referenced only by this undecodable record.
	if err := store.PutRawUnitRecord(other, 1, []byte{0xff, 0xff}); err != nil {
		t.Fatalf("PutRawUnitRecord: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("close other db: %v", err)
	}

	logf, logged := captureLogf()
	stats, ran := RunCASGC(logf, casPath, ownPath, own, true)
	if ran {
		t.Error("RunCASGC(force=true) ran = true with an undecodable record in another candidate database, want false (deferred)")
	}
	if stats != (store.GCStats{}) {
		t.Errorf("RunCASGC stats = %+v, want zero value when deferred", stats)
	}
	if !cas.Has(1) {
		t.Error("blob 1 was swept despite an undecodable other-DB record making the mark set incomplete, want kept")
	}
	if !loggedLineContains(*logged, otherPath, "undecodable") {
		t.Errorf("logged lines %v do not name %s's decode failure", *logged, otherPath)
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
