package xref

import (
	"bytes"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

func TestUnitCache_GetMiss(t *testing.T) {
	c := newUnitCache()
	if _, ok := c.get(1); ok {
		t.Errorf("get on an empty cache returned ok=true, want false")
	}
	if hits, misses := c.stats(); hits != 0 || misses != 1 {
		t.Errorf("stats = (hits=%d, misses=%d), want (0, 1)", hits, misses)
	}
}

func TestUnitCache_PutThenGetHits(t *testing.T) {
	c := newUnitCache()
	want := store.UnitBlob{Facts: []byte("facts")}
	c.put(42, &want)

	got, ok := c.get(42)
	if !ok {
		t.Fatalf("get(42) = ok=false, want a cached entry")
	}
	if !bytes.Equal(got.Facts, want.Facts) {
		t.Errorf("get(42).Facts = %q, want %q", got.Facts, want.Facts)
	}
	if hits, misses := c.stats(); hits != 1 || misses != 0 {
		t.Errorf("stats = (hits=%d, misses=%d), want (1, 0)", hits, misses)
	}
}

// TestUnitCache_EvictsLeastRecentlyUsed exercises LRU eviction against
// unitCache's actual (fixed, package-level) unitCacheCapacity: fills the
// cache to capacity, touches the oldest entry to make it MRU, then inserts
// one more entry and confirms the entry that was NOT touched (the new
// least-recently-used one) is the one evicted.
func TestUnitCache_EvictsLeastRecentlyUsed(t *testing.T) {
	c := newUnitCache()
	for i := 0; i < unitCacheCapacity; i++ {
		c.put(uint64(i), &store.UnitBlob{})
	}
	// Touch key 0 so it becomes MRU; key 1 is now the least recently used.
	if _, ok := c.get(0); !ok {
		t.Fatalf("get(0) = ok=false, want a cached entry")
	}

	c.put(uint64(unitCacheCapacity), &store.UnitBlob{})

	if _, ok := c.get(0); !ok {
		t.Errorf("get(0) after eviction = ok=false, want still cached (was touched most recently)")
	}
	if _, ok := c.get(1); ok {
		t.Errorf("get(1) after eviction = ok=true, want evicted (was the least recently used)")
	}
	if _, ok := c.get(unitCacheCapacity); !ok {
		t.Errorf("get(%d) = ok=false, want the just-inserted entry to be cached", unitCacheCapacity)
	}
}

func TestUnitCache_PutExistingKeyIsNoOp(t *testing.T) {
	c := newUnitCache()
	first := store.UnitBlob{Facts: []byte("first")}
	second := store.UnitBlob{Facts: []byte("second")}
	c.put(1, &first)
	c.put(1, &second)

	got, ok := c.get(1)
	if !ok {
		t.Fatalf("get(1) = ok=false, want a cached entry")
	}
	if string(got.Facts) != "first" {
		t.Errorf("get(1).Facts = %q, want %q (put on an existing key must not overwrite)", got.Facts, "first")
	}
}
