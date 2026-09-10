package graph

// This file regression-tests a fix to audit-silent-failures.md's M4: the
// shared graph cache key had no go.mod/go.sum content hash and no
// environment fingerprint, so a worktree on a different branch (or a
// differently-configured environment) could be served a contaminated
// snapshot from LoadCache with no way to detect the mismatch before
// revalidateGraph eventually corrected it in the background.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// newTestSnapshot returns an otherwise-empty Snapshot with buildFlagsFP set
// exactly as Load would set it for this test process's own environment, so
// a test exercising ModuleDigest alone does not also (incidentally, for the
// wrong reason) trip the unrelated BuildFlagsFP check LoadCache now also
// performs.
func newTestSnapshot() *Snapshot {
	return &Snapshot{
		Packages:     map[string]*Package{},
		buildFlagsFP: buildFlagsFingerprint(nil, os.Environ()),
	}
}

// TestLoadCache_RejectsModuleContentChangeEvenWithOlderMtime verifies
// LoadCache's ModuleDigest check catches a go.mod content change Stale's
// mtime-only comparison would miss: the rewritten go.mod is deliberately
// back-dated well before the cache file's own mtime, reproducing exactly
// the blind spot Stale's own doc describes for a cache SHARED across
// worktrees that can be on different branches with unrelated module
// content but similar file timestamps.
func TestLoadCache_RejectsModuleContentChangeEvenWithOlderMtime(t *testing.T) {
	root := t.TempDir()
	withTestCacheDir(t)
	goMod := filepath.Join(root, "go.mod")
	if err := os.WriteFile(goMod, []byte("module example.com/modchange\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	snap := newTestSnapshot()
	patterns := []string{"./..."}
	if err := SaveCache(root, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if _, ok := LoadCache(root, patterns, nil); !ok {
		t.Fatal("LoadCache = not ok immediately after SaveCache with go.mod unchanged, want a hit")
	}

	if err := os.WriteFile(goMod, []byte("module example.com/modchange\n\ngo 1.26\n\nrequire example.com/other v1.0.0\n"), 0o600); err != nil {
		t.Fatalf("rewrite go.mod: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(goMod, past, past); err != nil {
		t.Fatalf("chtimes go.mod: %v", err)
	}

	if Stale(root) {
		t.Fatal("Stale(root) = true after back-dating the rewritten go.mod, want false — this test needs Stale's own mtime blind spot to reproduce, not merely a newer mtime that Stale alone would already catch")
	}
	if _, ok := LoadCache(root, patterns, nil); ok {
		t.Error("LoadCache = ok despite go.mod content differing from what SaveCache recorded, want a miss (ModuleDigest should have caught it)")
	}
}

// TestLoadCache_RejectsGoWorkAppearingAfterSave verifies ModuleDigest also
// catches a tracked module file APPEARING (not just changing) since the
// cache was saved, distinct from TestStale_DeletedTrackedFile's own
// coverage of the opposite direction — Stale's own ModuleFiles bookkeeping
// only ever recorded what existed at save time, so it never needed (and
// does not have) a way to notice a brand-new go.work.
func TestLoadCache_RejectsGoWorkAppearingAfterSave(t *testing.T) {
	root := t.TempDir()
	withTestCacheDir(t)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/goworkappears\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	snap := newTestSnapshot()
	patterns := []string{"./..."}
	if err := SaveCache(root, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if _, ok := LoadCache(root, patterns, nil); !ok {
		t.Fatal("LoadCache = not ok immediately after SaveCache with go.work absent (as it was at save time), want a hit")
	}

	goWork := filepath.Join(root, "go.work")
	if err := os.WriteFile(goWork, []byte("go 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(goWork, past, past); err != nil {
		t.Fatalf("chtimes go.work: %v", err)
	}

	if _, ok := LoadCache(root, patterns, nil); ok {
		t.Error("LoadCache = ok after go.work appeared since SaveCache, want a miss")
	}
}

// otherGOARCH returns a GOARCH value distinct from runtime.GOARCH, for
// simulating a differently-configured loading environment.
func otherGOARCH() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

// TestLoadCache_RejectsBuildFlagsFingerprintMismatch verifies LoadCache
// recomputes buildFlagsFingerprint against the CURRENT process environment
// and rejects a cache saved under a different one, rather than only ever
// round-tripping whatever BuildFlagsFP happened to be saved (which is all
// the pre-fix code did — see TestCache_RoundTrip_PreservesBuildFlagsFingerprint
// in buildflagsfingerprint_test.go, which still must keep passing since
// that round trip is unaffected when the environment has NOT changed).
func TestLoadCache_RejectsBuildFlagsFingerprintMismatch(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "simple"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	withTestCacheDir(t)

	snap := loadTestdata(t)
	patterns := []string{"./..."}
	if err := SaveCache(root, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if _, ok := LoadCache(root, patterns, nil); !ok {
		t.Fatal("LoadCache = not ok immediately after SaveCache with the environment unchanged, want a hit")
	}

	t.Setenv("GOARCH", otherGOARCH())

	if _, ok := LoadCache(root, patterns, nil); ok {
		t.Error("LoadCache = ok despite GOARCH differing from the environment SaveCache ran under, want a miss")
	}
}
