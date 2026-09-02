package xref

import (
	"bytes"
	"context"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

func TestUnitCache_GetMiss(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
	if _, ok := c.get(1); ok {
		t.Errorf("get on an empty cache returned ok=true, want false")
	}
	if hits, misses := c.stats(); hits != 0 || misses != 1 {
		t.Errorf("stats = (hits=%d, misses=%d), want (0, 1)", hits, misses)
	}
}

func TestUnitCache_PutThenGetHits(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
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

// TestUnitCache_PutCopiesAndDropsUnreadFields confirms put retains neither
// a reference to u's original Facts/Export backing array nor its
// Files/Index fields: mutating u after put must not affect the cached
// copy, and a cached entry must report only Facts/Export bytes toward its
// size, not Files/Index (see unitCache's doc for why retaining the
// original slices would pin far more memory than callers ever read back
// out).
func TestUnitCache_PutCopiesAndDropsUnreadFields(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
	facts := []byte("facts")
	export := []byte("export")
	u := &store.UnitBlob{
		Facts:  facts,
		Export: export,
		Files:  []store.FileStat{{Path: "a.go", Size: 1}},
	}
	c.put(1, u)

	// Mutate the original backing arrays; a copying put must be unaffected.
	facts[0] = 'X'
	export[0] = 'X'

	got, ok := c.get(1)
	if !ok {
		t.Fatalf("get(1) = ok=false, want a cached entry")
	}
	if string(got.Facts) != "facts" {
		t.Errorf("get(1).Facts = %q, want %q (put must copy, not alias, the caller's slice)", got.Facts, "facts")
	}
	if string(got.Export) != "export" {
		t.Errorf("get(1).Export = %q, want %q (put must copy, not alias, the caller's slice)", got.Export, "export")
	}
	if got.Files != nil {
		t.Errorf("get(1).Files = %v, want nil (unitCache never reads Files, so it must not retain it)", got.Files)
	}
}

// TestUnitCache_PutEvictsByByteBound exercises the total-byte bound
// directly: entries each large enough that unitCacheCapacity would never
// trip first, so filling past maxBytes must evict solely on size.
func TestUnitCache_PutEvictsByByteBound(t *testing.T) {
	const entryPayload = 1000 // facts+export combined, before entrySizeOverhead
	const maxBytes = 10 * (entryPayload + entrySizeOverhead)
	c := newUnitCache(maxBytes)

	for i := 0; i < 20; i++ {
		u := &store.UnitBlob{
			Facts:  bytes.Repeat([]byte{byte(i)}, entryPayload/2),
			Export: bytes.Repeat([]byte{byte(i)}, entryPayload/2),
		}
		c.put(uint64(i), u)
		if c.numBytes > c.maxBytes {
			t.Fatalf("after put(%d): numBytes=%d exceeds maxBytes=%d", i, c.numBytes, c.maxBytes)
		}
	}

	if c.order.Len() > 10 {
		t.Errorf("order.Len() = %d, want at most 10 entries under a %d-byte bound with ~%d bytes/entry", c.order.Len(), maxBytes, entryPayload+entrySizeOverhead)
	}
	// The most recently inserted entries must have survived eviction.
	if _, ok := c.get(19); !ok {
		t.Errorf("get(19) = ok=false, want the most recently inserted entry to survive")
	}
	if _, ok := c.get(0); ok {
		t.Errorf("get(0) = ok=true, want the earliest entry evicted by the byte bound")
	}
}

// TestUnitCache_ByteBoundSurvivesOneOversizedEntry confirms a single entry
// larger than maxBytes does not wedge the cache: it is still admitted
// (put never rejects an entry outright), evicting everything else first,
// leaving the cache temporarily over its nominal bound by that one entry
// alone rather than looping forever trying to make room.
func TestUnitCache_ByteBoundSurvivesOneOversizedEntry(t *testing.T) {
	c := newUnitCache(100)
	small := &store.UnitBlob{Facts: []byte("x")}
	c.put(1, small)

	big := &store.UnitBlob{Facts: bytes.Repeat([]byte("y"), 1000)}
	c.put(2, big)

	if _, ok := c.get(1); ok {
		t.Errorf("get(1) = ok=true, want evicted to make room for the oversized entry")
	}
	if _, ok := c.get(2); !ok {
		t.Errorf("get(2) = ok=false, want the oversized entry admitted anyway")
	}
}

// TestUnitCache_EvictsLeastRecentlyUsed exercises LRU eviction against
// unitCache's actual (fixed, package-level) unitCacheCapacity: fills the
// cache to capacity, touches the oldest entry to make it MRU, then inserts
// one more entry and confirms the entry that was NOT touched (the new
// least-recently-used one) is the one evicted.
func TestUnitCache_EvictsLeastRecentlyUsed(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
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

// TestUnitCache_PutFactsThenGetFactsHits verifies putFacts/getFacts work
// as their own facts-only cache path, independent of get/put's full-blob
// path — the caching layer References' closure walk (locationsForAll) uses
// via Resolver.unitFacts.
func TestUnitCache_PutFactsThenGetFactsHits(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
	c.putFacts(42, []byte("facts"))

	got, ok := c.getFacts(42)
	if !ok {
		t.Fatalf("getFacts(42) = ok=false, want a cached entry")
	}
	if string(got) != "facts" {
		t.Errorf("getFacts(42) = %q, want %q", got, "facts")
	}
	if hits, misses := c.stats(); hits != 1 || misses != 0 {
		t.Errorf("stats = (hits=%d, misses=%d), want (1, 0)", hits, misses)
	}
}

// TestUnitCache_GetMissesFactsOnlyEntry verifies get (the full-blob path)
// reports a miss for a key cached only via putFacts: a caller that needs
// Export must never receive a zero-value Export indistinguishable from a
// genuinely empty one.
func TestUnitCache_GetMissesFactsOnlyEntry(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
	c.putFacts(1, []byte("facts"))

	if _, ok := c.get(1); ok {
		t.Errorf("get(1) on a facts-only entry = ok=true, want false (Export was never fetched)")
	}
}

// TestUnitCache_PutUpgradesFactsOnlyEntry verifies put, called on a key
// already cached via putFacts, upgrades that entry in place (adding
// Export) rather than leaving it stuck facts-only or duplicating it: after
// the upgrade, both get and getFacts must see the same, now-complete entry.
func TestUnitCache_PutUpgradesFactsOnlyEntry(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
	c.putFacts(1, []byte("facts"))
	before := c.numBytes

	full := &store.UnitBlob{Facts: []byte("facts"), Export: []byte("export")}
	c.put(1, full)

	if c.numBytes <= before {
		t.Errorf("numBytes after upgrade = %d, want > %d (Export bytes now accounted)", c.numBytes, before)
	}
	got, ok := c.get(1)
	if !ok {
		t.Fatalf("get(1) after upgrade = ok=false, want true")
	}
	if string(got.Facts) != "facts" || string(got.Export) != "export" {
		t.Errorf("get(1) after upgrade = %+v, want Facts=%q Export=%q", got, "facts", "export")
	}
	if c.order.Len() != 1 {
		t.Errorf("order.Len() after upgrade = %d, want 1 (upgrade must not duplicate the entry)", c.order.Len())
	}
}

// TestUnitCache_PutFactsExistingKeyIsNoOp verifies putFacts never
// overwrites an existing entry (facts-only or full) — mirroring put's own
// TestUnitCache_PutExistingKeyIsNoOp, since putFacts has nothing new to add
// once a key is already cached in either form.
func TestUnitCache_PutFactsExistingKeyIsNoOp(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
	full := &store.UnitBlob{Facts: []byte("facts"), Export: []byte("export")}
	c.put(1, full)
	c.putFacts(1, []byte("different-facts"))

	got, ok := c.get(1)
	if !ok {
		t.Fatalf("get(1) = ok=false, want a cached entry")
	}
	if string(got.Export) != "export" {
		t.Errorf("get(1).Export = %q, want %q (putFacts on an existing full entry must not touch it)", got.Export, "export")
	}
}

// TestResolverUnitFacts_ThenUnitBlobUpgradesLiveCache is unitFacts/unitBlob's
// own integration counterpart of TestUnitCache_PutUpgradesFactsOnlyEntry:
// driven through a real Resolver (rather than a synthetic *unitCache
// directly), it pins that a References-style Facts-only read followed by a
// Definition-style full read ends up sharing ONE cache entry per package,
// upgraded in place, rather than caching the same content twice.
func TestResolverUnitFacts_ThenUnitBlobUpgradesLiveCache(t *testing.T) {
	r, _ := newTestResolver(t)
	ctx := context.Background()
	pkgHash := store.Hash(pkgImpl)

	if _, err := r.unitFacts(ctx, pkgHash); err != nil {
		t.Fatalf("unitFacts (first, populates facts-only entry): %v", err)
	}
	if _, err := r.unitFacts(ctx, pkgHash); err != nil {
		t.Fatalf("unitFacts (second, should hit): %v", err)
	}
	hits, misses := r.units.stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("stats after two unitFacts calls = (hits=%d, misses=%d), want (1, 1)", hits, misses)
	}

	if _, err := r.unitBlob(ctx, pkgHash); err != nil {
		t.Fatalf("unitBlob (upgrades the facts-only entry): %v", err)
	}
	if hits, misses = r.units.stats(); hits != 1 || misses != 2 {
		t.Fatalf("stats after unitBlob on a facts-only entry = (hits=%d, misses=%d), want (1, 2) (get() must miss a facts-only entry)", hits, misses)
	}

	if _, err := r.unitBlob(ctx, pkgHash); err != nil {
		t.Fatalf("unitBlob (second, should hit the now-upgraded entry): %v", err)
	}
	if hits, misses = r.units.stats(); hits != 2 || misses != 2 {
		t.Fatalf("stats after second unitBlob = (hits=%d, misses=%d), want (2, 2) (the upgrade must have stuck)", hits, misses)
	}
}

func TestUnitCache_PutExistingKeyIsNoOp(t *testing.T) {
	c := newUnitCache(defaultUnitCacheBytes)
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
