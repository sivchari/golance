package store

import (
	"bytes"
	"context"
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

	if _, ok, err := cas.Get(context.Background(), key); err != nil || ok {
		t.Fatalf("Get() before Put = (%v, %v), want (false, nil)", ok, err)
	}
	if cas.Has(key) {
		t.Error("Has() before Put = true, want false")
	}

	want := []byte("unit-blob-contents")
	if err := cas.Put(key, want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, ok, err := cas.Get(context.Background(), key)
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

	got, ok, err := cas.Get(context.Background(), key)
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

// TestCASGCSweepsUnreferencedKeepsMarkedAndYoung verifies GC's core
// mark-and-sweep property in one pass: a marked-but-old blob and an
// unmarked-but-young blob both survive, while an unmarked, old blob is
// swept.
func TestCASGCSweepsUnreferencedKeepsMarkedAndYoung(t *testing.T) {
	cas := openTestCAS(t)
	for key, content := range map[uint64]string{
		1: "referenced-old",
		2: "unreferenced-old",
		3: "unreferenced-young",
	} {
		if err := cas.Put(key, []byte(content)); err != nil {
			t.Fatalf("Put(%d): %v", key, err)
		}
	}
	old := time.Now().Add(-GraceWindow - time.Hour)
	for _, key := range []uint64{1, 2} {
		if err := os.Chtimes(cas.blobPath(key), old, old); err != nil {
			t.Fatalf("Chtimes(%d): %v", key, err)
		}
	}
	// key 3 keeps its just-written (young) mtime.

	marks := map[uint64]struct{}{1: {}}
	stats, err := cas.GC(time.Now(), marks)
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if stats.SweptCount != 1 {
		t.Errorf("GC() SweptCount = %d, want 1", stats.SweptCount)
	}
	if stats.KeptCount != 2 {
		t.Errorf("GC() KeptCount = %d, want 2", stats.KeptCount)
	}

	if !cas.Has(1) {
		t.Error("Has(1) = false after GC, want true (marked, must survive regardless of age)")
	}
	if cas.Has(2) {
		t.Error("Has(2) = true after GC, want false (unmarked and past GraceWindow)")
	}
	if !cas.Has(3) {
		t.Error("Has(3) = false after GC, want true (unmarked but within GraceWindow)")
	}
}

// TestCASGCReportsByteCounts verifies GCStats' byte totals match the actual
// blob sizes, not just counts — internal/server's log line reports both.
func TestCASGCReportsByteCounts(t *testing.T) {
	cas := openTestCAS(t)
	kept := []byte("0123456789") // 10 bytes, marked
	swept := []byte("abc")       // 3 bytes, unmarked and old
	if err := cas.Put(1, kept); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	if err := cas.Put(2, swept); err != nil {
		t.Fatalf("Put(2): %v", err)
	}
	old := time.Now().Add(-GraceWindow - time.Hour)
	if err := os.Chtimes(cas.blobPath(2), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	stats, err := cas.GC(time.Now(), map[uint64]struct{}{1: {}})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if stats.KeptBytes != int64(len(kept)) {
		t.Errorf("GC() KeptBytes = %d, want %d", stats.KeptBytes, len(kept))
	}
	if stats.SweptBytes != int64(len(swept)) {
		t.Errorf("GC() SweptBytes = %d, want %d", stats.SweptBytes, len(swept))
	}
}

// TestCASGCConcurrentGetOfSweptBlobIsAnOrdinaryMiss verifies the safety
// argument (*CAS).GC's doc relies on: a blob removed by GC because it was
// unmarked is indistinguishable, from a subsequent Get's point of view,
// from a key that was simply never Put — an ordinary cache miss, never an
// error — exactly the race a concurrent build racing a GC pass over the
// same key would hit.
func TestCASGCConcurrentGetOfSweptBlobIsAnOrdinaryMiss(t *testing.T) {
	cas := openTestCAS(t)
	if err := cas.Put(1, []byte("stale")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	old := time.Now().Add(-GraceWindow - time.Hour)
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if _, err := cas.GC(time.Now(), nil); err != nil {
		t.Fatalf("GC() error = %v", err)
	}

	got, ok, err := cas.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get() after GC swept key 1 error = %v, want a plain miss (ok=false, err=nil)", err)
	}
	if ok {
		t.Fatalf("Get() after GC swept key 1 = (%q, true), want ok=false", got)
	}
}

func TestCASMaybeGCRespectsInterval(t *testing.T) {
	cas := openTestCAS(t)
	if err := cas.Put(1, []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	old := time.Now().Add(-GraceWindow - time.Hour)
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// First call: stamp file does not exist yet, so this actually sweeps.
	stats, ran, err := cas.MaybeGC(time.Now(), nil)
	if err != nil {
		t.Fatalf("MaybeGC() error = %v", err)
	}
	if !ran {
		t.Fatal("MaybeGC() ran = false on first call, want true")
	}
	if stats.SweptCount != 1 {
		t.Errorf("MaybeGC() SweptCount = %d, want 1", stats.SweptCount)
	}
	if cas.Has(1) {
		t.Fatal("Has(1) = true after first MaybeGC, want false")
	}

	// Recreate the stale blob and re-stamp its old mtime, then call
	// MaybeGC again immediately: it must be a no-op (interval not
	// elapsed), so the blob survives.
	if err := cas.Put(1, []byte("old-again")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	_, ran, err = cas.MaybeGC(time.Now(), nil)
	if err != nil {
		t.Fatalf("MaybeGC() second call error = %v", err)
	}
	if ran {
		t.Error("MaybeGC() ran = true within GCInterval, want false (must not have re-walked)")
	}
	if !cas.Has(1) {
		t.Error("Has(1) = false after a MaybeGC call within GCInterval, want true (must not have re-walked)")
	}
}

// TestCASGCAgedIgnoresMarksSweepsByAgeAlone verifies GCAged's contract for
// a CAS directory with no mark set at all (internal/depexport's
// machine-global cache, which no per-repo index database ever references):
// a blob older than maxAge is swept regardless of any marks map passed in,
// and a young one survives — marks is accepted only so GCAged can share
// sweep's implementation with GC, never actually consulted meaningfully
// here.
func TestCASGCAgedIgnoresMarksSweepsByAgeAlone(t *testing.T) {
	cas := openTestCAS(t)
	const maxAge = 30 * 24 * time.Hour
	for key, content := range map[uint64]string{
		1: "old",
		2: "young",
	} {
		if err := cas.Put(key, []byte(content)); err != nil {
			t.Fatalf("Put(%d): %v", key, err)
		}
	}
	old := time.Now().Add(-maxAge - time.Hour)
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes(1): %v", err)
	}

	stats, err := cas.GCAged(time.Now(), maxAge)
	if err != nil {
		t.Fatalf("GCAged() error = %v", err)
	}
	if stats.SweptCount != 1 {
		t.Errorf("GCAged() SweptCount = %d, want 1", stats.SweptCount)
	}
	if stats.KeptCount != 1 {
		t.Errorf("GCAged() KeptCount = %d, want 1", stats.KeptCount)
	}
	if cas.Has(1) {
		t.Error("Has(1) = true after GCAged, want false (older than maxAge)")
	}
	if !cas.Has(2) {
		t.Error("Has(2) = false after GCAged, want true (younger than maxAge)")
	}
}

func TestCASMaybeGCAgedRespectsInterval(t *testing.T) {
	cas := openTestCAS(t)
	const maxAge = 30 * 24 * time.Hour
	const interval = 7 * 24 * time.Hour
	if err := cas.Put(1, []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	old := time.Now().Add(-maxAge - time.Hour)
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	stats, ran, err := cas.MaybeGCAged(time.Now(), maxAge, interval)
	if err != nil {
		t.Fatalf("MaybeGCAged() error = %v", err)
	}
	if !ran {
		t.Fatal("MaybeGCAged() ran = false on first call, want true")
	}
	if stats.SweptCount != 1 {
		t.Errorf("MaybeGCAged() SweptCount = %d, want 1", stats.SweptCount)
	}
	if cas.Has(1) {
		t.Fatal("Has(1) = true after first MaybeGCAged, want false")
	}

	if err := cas.Put(1, []byte("old-again")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Chtimes(cas.blobPath(1), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	_, ran, err = cas.MaybeGCAged(time.Now(), maxAge, interval)
	if err != nil {
		t.Fatalf("MaybeGCAged() second call error = %v", err)
	}
	if ran {
		t.Error("MaybeGCAged() ran = true within interval, want false (must not have re-walked)")
	}
	if !cas.Has(1) {
		t.Error("Has(1) = false after a MaybeGCAged call within interval, want true (must not have re-walked)")
	}
}
