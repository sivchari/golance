package store

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestCAS(t *testing.T) *CAS {
	t.Helper()
	cas, err := OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("OpenCAS() error = %v", err)
	}
	return cas
}

func TestCASPutGetRoundTrip(t *testing.T) {
	cas := openTestCAS(t)
	const key = 12345

	if _, ok, err := cas.Get(key); err != nil || ok {
		t.Fatalf("Get() before Put = (%v, %v), want (false, nil)", ok, err)
	}
	if cas.Has(key) {
		t.Error("Has() before Put = true, want false")
	}

	want := []byte("unit-blob-contents")
	if err := cas.Put(key, want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, ok, err := cas.Get(key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok {
		t.Fatal("Get() ok = false after Put, want true")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get() = %q, want %q", got, want)
	}
	if !cas.Has(key) {
		t.Error("Has() after Put = false, want true")
	}
}

// TestCASConcurrentPutSameKeySameContent verifies that many goroutines (a
// stand-in for many processes/worktrees) racing to Put the same key with
// identical content — the only case that can happen, since the key is a
// deterministic function of the content — never corrupts the stored blob
// and never errors.
func TestCASConcurrentPutSameKeySameContent(t *testing.T) {
	cas := openTestCAS(t)
	const key = 42
	content := make([]byte, 64*1024)
	for i := range content {
		content[i] = byte(i)
	}

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = cas.Put(key, content)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Put() error = %v", i, err)
		}
	}

	got, ok, err := cas.Get(key)
	if err != nil || !ok {
		t.Fatalf("Get() after concurrent Put = (%v, %v, %v), want (data, true, nil)", got, ok, err)
	}
	if !bytes.Equal(got, content) {
		t.Error("Get() after concurrent Put returned corrupted content")
	}

	// No leftover temp files: every writer's temp file was either renamed
	// into place or cleaned up.
	shardDir := filepath.Dir(cas.blobPath(key))
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", shardDir, err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("shard directory has %d entries, want 1 (no leftover temp files): %v", len(entries), names)
	}
}

func TestCASTrimRemovesOnlyStaleBlobs(t *testing.T) {
	cas := openTestCAS(t)
	if err := cas.Put(1, []byte("old")); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	if err := cas.Put(2, []byte("fresh")); err != nil {
		t.Fatalf("Put(2): %v", err)
	}

	old := time.Now().Add(-TrimMaxAge - time.Hour)
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := cas.Trim(time.Now(), TrimMaxAge)
	if err != nil {
		t.Fatalf("Trim() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("Trim() removed = %d, want 1", removed)
	}
	if cas.Has(1) {
		t.Error("Has(1) = true after Trim, want false (stale)")
	}
	if !cas.Has(2) {
		t.Error("Has(2) = false after Trim, want true (fresh, must survive)")
	}
}

// TestCASTrimSpareUsedBlob verifies that a blob whose only recent activity
// is a Has (not a Get) still survives a Trim — presence-checking a blob
// during revalidation counts as "in use" (see CAS.Has's doc), exactly as
// important as a full Get for GC correctness.
func TestCASTrimSpareUsedBlob(t *testing.T) {
	cas := openTestCAS(t)
	if err := cas.Put(1, []byte("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	old := time.Now().Add(-TrimMaxAge - time.Hour)
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if !cas.Has(1) {
		t.Fatal("Has(1) = false, want true")
	}
	// Has must have refreshed the mtime past the cutoff.
	removed, err := cas.Trim(time.Now(), TrimMaxAge)
	if err != nil {
		t.Fatalf("Trim() error = %v", err)
	}
	if removed != 0 {
		t.Errorf("Trim() removed = %d, want 0 (Has should have refreshed mtime)", removed)
	}
}

func TestCASMaybeTrimRespectsInterval(t *testing.T) {
	cas := openTestCAS(t)
	if err := cas.Put(1, []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	old := time.Now().Add(-TrimMaxAge - time.Hour)
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// First call: stamp file does not exist yet, so this actually trims.
	if err := cas.MaybeTrim(time.Now()); err != nil {
		t.Fatalf("MaybeTrim() error = %v", err)
	}
	if cas.Has(1) {
		t.Fatal("Has(1) = true after first MaybeTrim, want false")
	}

	// Recreate the stale blob and re-stamp its old mtime, then call
	// MaybeTrim again immediately: it must be a no-op (interval not
	// elapsed), so the blob survives.
	if err := cas.Put(1, []byte("old-again")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := cas.MaybeTrim(time.Now()); err != nil {
		t.Fatalf("MaybeTrim() second call error = %v", err)
	}
	if !cas.Has(1) {
		t.Error("Has(1) = false after a MaybeTrim call within TrimInterval, want true (must not have re-walked)")
	}
}
