package xref

import (
	"container/list"
	"sync"

	"github.com/sivchari/golance/internal/store"
)

// unitCacheCapacity bounds how many decoded store.UnitBlob values
// unitCache holds at once. A single Implementation query over a large
// monorepo can legitimately touch on the order of a few hundred candidate
// packages before intersection/confirmation narrows them down (e.g. the
// production report's LookupMethod-by-name count of 200 for a single
// method name) — this is sized comfortably above that so one query's own
// candidate set stays warm for its own duration, without unboundedly
// growing memory the way caching every package a long session ever visits
// would.
const unitCacheCapacity = 512

// unitCache is a fixed-capacity, least-recently-used cache of decoded
// store.UnitBlob values keyed by their store.UnitPointer.BlobKey — the CAS
// content-address a blob's exact bytes hash to (see store.CAS's doc).
// Keying by content hash rather than by package makes invalidation
// implicit: a reindexed package gets a new BlobKey, so its old cache entry
// simply stops being looked up (and eventually ages out via normal LRU
// eviction) instead of needing to be explicitly dropped the way
// Resolver.Invalidate drops typecheck.Cache entries.
//
// Safe for concurrent use: Resolver's own methods are called concurrently
// by the server's request dispatch (see Resolver's doc), so every access
// below is serialized by mu, mirroring internal/typecheck.Cache's own
// mutex-protected map.
type unitCache struct {
	mu    sync.Mutex
	order *list.List // MRU at Front, LRU at Back; element.Value is *unitCacheEntry
	index map[uint64]*list.Element

	hits   int64 // get calls that found an entry
	misses int64 // get calls that found nothing
}

type unitCacheEntry struct {
	blobKey uint64
	unit    store.UnitBlob
}

// newUnitCache returns an empty unitCache holding at most unitCacheCapacity
// entries.
func newUnitCache() *unitCache {
	return &unitCache{order: list.New(), index: make(map[uint64]*list.Element, unitCacheCapacity)}
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
	return entry.unit, true
}

// stats returns the running hit/miss counts of every get call so far, for
// test observability (mirroring internal/typecheck.Cache.Decodes' role of
// asserting a warm cache avoids redundant decode work).
func (c *unitCache) stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// put inserts blobKey's decoded UnitBlob, evicting the least recently used
// entry first if the cache is already at capacity. put is a no-op if
// blobKey is already cached — every caller only ever puts a value it just
// decoded after a get miss, under this same lock, so a duplicate put here
// would only mean a concurrent goroutine decoded the identical
// content-addressed bytes first, in which case keeping its (byte-identical)
// entry is fine.
func (c *unitCache) put(blobKey uint64, u store.UnitBlob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[blobKey]; ok {
		c.order.MoveToFront(el)
		return
	}
	if c.order.Len() >= unitCacheCapacity {
		c.evictOldest()
	}
	el := c.order.PushFront(&unitCacheEntry{blobKey: blobKey, unit: u})
	c.index[blobKey] = el
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
}
