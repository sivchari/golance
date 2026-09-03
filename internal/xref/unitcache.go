package xref

import (
	"bytes"
	"container/list"
	"sync"

	"github.com/sivchari/golance/internal/store"
)

// unitCacheCapacity bounds how many decoded store.UnitBlob values
// unitCache holds at once, on top of defaultUnitCacheBytes. A single
// Implementation query over a large monorepo can legitimately touch on the
// order of a few hundred candidate packages before intersection/
// confirmation narrows them down (e.g. the production report's
// LookupMethod-by-name count of 200 for a single method name) — this is
// sized comfortably above that so one query's own candidate set stays warm
// for its own duration. It exists alongside the byte bound (rather than
// instead of it) because a workspace of many small packages could
// otherwise stay well under the byte bound while still growing the
// map/list bookkeeping unboundedly.
const unitCacheCapacity = 512

// defaultUnitCacheBytes bounds the total retained size of unitCache's
// entries (see entrySizeOverhead for what "size" counts). 256MiB is
// comfortably above what one query's own candidate set needs to stay warm
// (see unitCacheCapacity's doc) while still bounding the multi-gigabyte
// growth a single large query was observed to cause in the field with no
// byte accounting at all: a query touching a few hundred candidate
// packages, each with a multi-MB export-data blob, otherwise pins
// gigabytes for the cache's entire lifetime. Override via WithUnitCacheBytes.
const defaultUnitCacheBytes = 256 << 20

// entrySizeOverhead is a small fixed cost added to each entry's accounted
// size on top of its byte-slice lengths, covering the list.Element, map
// entry, and unitCacheEntry struct golance's own bookkeeping allocates per
// entry — deliberately approximate (see unitCacheEntry.size's doc), just
// enough that a cache full of many tiny entries cannot look free.
const entrySizeOverhead = 64

// unitCache is a least-recently-used cache of decoded store.UnitBlob
// values, bounded by both entry count (unitCacheCapacity) and total
// retained bytes (maxBytes), keyed by their store.UnitPointer.BlobKey —
// the CAS content address a blob's exact bytes hash to (see store.CAS's
// doc). Keying by content hash rather than by package makes invalidation
// implicit: a reindexed package gets a new BlobKey, so its old cache entry
// simply stops being looked up (and eventually ages out via normal LRU
// eviction) instead of needing to be explicitly dropped the way
// Resolver.Invalidate drops typecheck.Cache entries.
//
// Entries hold only Facts and Export, copied out of the store.UnitBlob a
// caller decodes — the only two fields any unitBlob caller reads (see its
// doc) — rather than the whole decoded value. This matters for two
// reasons: store.DecodeUnitBlob's Facts/Export alias its input blob's
// backing array without copying (see its doc), so keeping the original,
// uncopied slices would retain the WHOLE blob (including its Files/Index
// sections, decoded into memory only to be discarded here) for as long as
// the entry survives, no matter how small a slice of it Facts/Export
// individually are; copying breaks that aliasing so an entry retains
// (and size accounts for) only the bytes its callers actually use.
//
// Safe for concurrent use: Resolver's own methods are called concurrently
// by the server's request dispatch (see Resolver's doc), so every access
// below is serialized by mu, mirroring internal/typecheck.Cache's own
// mutex-protected map.
type unitCache struct {
	mu       sync.Mutex
	order    *list.List // MRU at Front, LRU at Back; element.Value is *unitCacheEntry
	index    map[uint64]*list.Element
	maxBytes int64
	numBytes int64 // sum of every currently-held entry's size()

	hits   int64 // get calls that found an entry
	misses int64 // get calls that found nothing
}

type unitCacheEntry struct {
	blobKey uint64
	facts   []byte
	export  []byte
}

// size estimates entry's retained heap footprint: its two owned byte
// slices (copied, not aliased — see unitCache's doc, so this is their true
// retained size) plus entrySizeOverhead's fixed per-entry bookkeeping
// cost. Deliberately approximate — it ignores things like slice header
// padding or GC bookkeeping — but good enough to bound total cache growth
// to the right order of magnitude, the same standard internal/typecheck.
// Cache.Bytes' doc describes for its own naive byte proxy.
func (e *unitCacheEntry) size() int64 {
	return int64(len(e.facts)) + int64(len(e.export)) + entrySizeOverhead
}

// newUnitCache returns an empty unitCache bounded by unitCacheCapacity
// entries and maxBytes total accounted size.
func newUnitCache(maxBytes int64) *unitCache {
	return &unitCache{
		order:    list.New(),
		index:    make(map[uint64]*list.Element, unitCacheCapacity),
		maxBytes: maxBytes,
	}
}

// get returns blobKey's cached UnitBlob, if present, moving it to the front
// (most recently used).
func (c *unitCache) get(blobKey uint64) (store.UnitBlob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[blobKey]
	if !ok {
		c.misses++
		return store.UnitBlob{}, false
	}
	c.hits++
	c.order.MoveToFront(el)
	entry, _ := el.Value.(*unitCacheEntry)
	return store.UnitBlob{Facts: entry.facts, Export: entry.export}, true
}

// stats returns the running hit/miss counts of every get call so far, for
// test observability (mirroring internal/typecheck.Cache.Decodes' role of
// asserting a warm cache avoids redundant decode work).
func (c *unitCache) stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// put copies blobKey's decoded UnitBlob's Facts and Export (see
// unitCache's doc for why only those two fields, and why copied) and
// inserts them, evicting least-recently-used entries first until both the
// entry-count and total-bytes bounds are satisfied. No-op if blobKey is
// already cached — every caller only ever puts a value it just decoded
// after a get miss, under this same lock, so a duplicate put here would
// only mean a concurrent goroutine decoded the identical content-addressed
// bytes first, in which case keeping its (byte-identical) entry is fine.
func (c *unitCache) put(blobKey uint64, u *store.UnitBlob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[blobKey]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.insert(&unitCacheEntry{blobKey: blobKey, facts: bytes.Clone(u.Facts), export: bytes.Clone(u.Export)})
}

// insert admits entry, evicting least-recently-used entries first until
// both the entry-count and total-bytes bounds are satisfied. Callers hold
// c.mu and have already confirmed entry.blobKey is not present.
func (c *unitCache) insert(entry *unitCacheEntry) {
	size := entry.size()
	for c.order.Len() > 0 && (c.order.Len() >= unitCacheCapacity || c.numBytes+size > c.maxBytes) {
		c.evictOldest()
	}

	el := c.order.PushFront(entry)
	c.index[entry.blobKey] = el
	c.numBytes += size
}

// evictOldest removes the least recently used entry, if any.
func (c *unitCache) evictOldest() {
	back := c.order.Back()
	if back == nil {
		return
	}
	entry, _ := back.Value.(*unitCacheEntry)
	delete(c.index, entry.blobKey)
	c.order.Remove(back)
	c.numBytes -= entry.size()
}
