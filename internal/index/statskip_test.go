package index

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/store"
)

// statOf stats path, failing the test on error.
func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

// TestBuild_UnchangedStatSkipsWithoutReadingContent verifies the stat-only
// fast path: when a file's (size, mtime) still match what the last Build
// recorded, a second Build never rereads its content at all. This is
// proven by corrupting the on-disk bytes (same length, so size still
// matches) while restoring the exact original mtime: if Build fell back to
// its content-hash check, it would either fail to parse the corrupted
// source or detect the hash mismatch and reprocess it — neither happens.
func TestBuild_UnchangedStatSkipsWithoutReadingContent(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}

	leafPath := filepath.Join(dir, "leaf", "leaf.go")
	before := statOf(t, leafPath)

	garbage := bytes.Repeat([]byte("X"), int(before.Size())) // same length, unparseable
	if err := os.WriteFile(leafPath, garbage, 0o600); err != nil {
		t.Fatalf("corrupt leaf.go: %v", err)
	}
	mtime := before.ModTime()
	if err := os.Chtimes(leafPath, mtime, mtime); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}

	stats, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if stats.Processed != 0 {
		t.Errorf("Processed = %d, want 0 (stat match must skip without reading the corrupted content)", stats.Processed)
	}
	if stats.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3", stats.Skipped)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
}

// TestBuild_TouchedFileSameContentSkipsAndRefreshesStat verifies the
// touch/checkout case: an mtime change with byte-identical content is
// still a skip (via the content-hash fallback), and the stat snapshot is
// refreshed afterward so a later run can skip by stat alone again.
func TestBuild_TouchedFileSameContentSkipsAndRefreshesStat(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}

	leafPath := filepath.Join(dir, "leaf", "leaf.go")
	before := statOf(t, leafPath)
	newMtime := before.ModTime().Add(1 * time.Hour)
	if err := os.Chtimes(leafPath, newMtime, newMtime); err != nil {
		t.Fatalf("touch leaf.go: %v", err)
	}

	stats, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if stats.Processed != 0 {
		t.Errorf("Processed = %d, want 0 (touch with identical content must not trigger a rebuild)", stats.Processed)
	}
	if stats.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3", stats.Skipped)
	}

	ptr, err := db.GetUnit(store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf): %v", err)
	}
	found := false
	for _, f := range ptr.Files {
		if f.Path == leafPath {
			found = true
			if f.ModTimeNanos != newMtime.UnixNano() {
				t.Errorf("stored mtime for leaf.go = %d, want refreshed to %d", f.ModTimeNanos, newMtime.UnixNano())
			}
		}
	}
	if !found {
		t.Errorf("ptr.Files for leaf has no entry for %s: %+v", leafPath, ptr.Files)
	}
}

// TestBuild_ContentChangeTriggersRebuild verifies that an actual content
// change (mtime necessarily changes too, since the file is rewritten) is
// reprocessed rather than skipped. This particular edit is body-only (the
// exported Greeting/String/Hello signatures are untouched), so leaf's
// export hash does not change, and — per the package doc's key composition
// — neither mid nor top (both of which import leaf) are forced to
// recheck: only leaf itself is Processed.
func TestBuild_ContentChangeTriggersRebuild(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	firstPtr, err := db.GetUnit(store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf): %v", err)
	}

	leafPath := filepath.Join(dir, "leaf", "leaf.go")
	edited := []byte(`// Package leaf has no workspace dependencies.
package leaf

// Greeting is a friendly greeting.
type Greeting struct {
	Message string
}

// String implements fmt.Stringer.
func (g Greeting) String() string {
	return g.Message
}

// Hello returns a Greeting for name, now with an exclamation mark.
func Hello(name string) Greeting {
	return Greeting{Message: "hello " + name + "!"}
}
`)
	if err := os.WriteFile(leafPath, edited, 0o600); err != nil {
		t.Fatalf("edit leaf.go: %v", err)
	}

	stats, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed = %d, want 1 (leaf only; the edit is body-only, so its export hash is unchanged)", stats.Processed)
	}
	if stats.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (mid, top: unaffected by a body-only edit to a dependency)", stats.Skipped)
	}

	secondPtr, err := db.GetUnit(store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf) after rebuild: %v", err)
	}
	if secondPtr.ContentHash == firstPtr.ContentHash {
		t.Error("ContentHash unchanged after editing leaf.go's content")
	}
	if secondPtr.ExportHash != firstPtr.ExportHash {
		t.Error("ExportHash changed after a body-only edit, want unchanged (same exported signatures)")
	}
}

// TestBuild_FileAddedOrRemovedTriggersRebuild verifies that adding or
// removing a file from a package (detected via the stored file count no
// longer matching the package's current GoFiles) forces that package back
// through the full rebuild path, even though every remaining file's own
// (size, mtime) is untouched.
func TestBuild_FileAddedOrRemovedTriggersRebuild(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}

	// unexported, so leaf's export hash (and so mid/top's own keys) stays
	// untouched: this test is about file-count-mismatch detection, not
	// export-hash propagation (see TestBuild_ContentChangeTriggersRebuild
	// for that).
	extraPath := filepath.Join(dir, "leaf", "extra.go")
	extraSrc := []byte("package leaf\n\n// extra is an additional helper.\nfunc extra() int { return 2 }\n")
	if err := os.WriteFile(extraPath, extraSrc, 0o600); err != nil {
		t.Fatalf("add extra.go: %v", err)
	}

	snap2 := loadSnapshot(t, dir)
	stats, err := Build(ctx, snap2, db, cas, Options{})
	if err != nil {
		t.Fatalf("Build after adding a file: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed after adding a file = %d, want 1 (leaf)", stats.Processed)
	}

	if err := os.Remove(extraPath); err != nil {
		t.Fatalf("remove extra.go: %v", err)
	}
	snap3 := loadSnapshot(t, dir)
	stats, err = Build(ctx, snap3, db, cas, Options{})
	if err != nil {
		t.Fatalf("Build after removing a file: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed after removing a file = %d, want 1 (leaf)", stats.Processed)
	}
}

// TestBuild_NoStatSnapshotFallsBackToContentHash verifies that a
// UnitPointer with no per-file stat snapshot at all (Files == nil, which
// can happen any time the best-effort stat call after a type-check fails —
// see processUnit) still correctly skips an unchanged package via the
// content-hash check, and repopulates Files so later runs can skip by stat
// alone.
func TestBuild_NoStatSnapshotFallsBackToContentHash(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	ptr, err := db.GetUnit(store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf): %v", err)
	}
	if len(ptr.Files) == 0 {
		t.Fatal("ptr.Files is empty right after a Build; test setup is broken")
	}

	// Simulate a pointer with no stat snapshot recorded.
	noStat := ptr
	noStat.Files = nil
	if err := db.PutUnit(&store.UnitEntry{PkgHash: store.Hash(pkgLeaf), Pointer: noStat}); err != nil {
		t.Fatalf("PutUnit(noStat): %v", err)
	}

	stats, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if stats.Processed != 0 {
		t.Errorf("Processed = %d, want 0 (content unchanged, must fall back to content hash without a stat snapshot)", stats.Processed)
	}
	if stats.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3", stats.Skipped)
	}

	refreshed, err := db.GetUnit(store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf) after refresh: %v", err)
	}
	if len(refreshed.Files) == 0 {
		t.Error("ptr.Files is still empty after a Build over a no-stat-snapshot pointer; want it repopulated")
	}
}
