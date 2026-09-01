package depcheck

import "container/list"

// lruCache is a fixed-capacity, least-recently-used cache of
// *CheckedPackage values keyed by import path. Not safe for concurrent use
// on its own — Provider serializes every call to it under its own mutex.
type lruCache struct {
	capacity int
	order    *list.List // MRU at Front, LRU at Back; element.Value is *lruEntry
	index    map[string]*list.Element
}

type lruEntry struct {
	pkgPath string
	cp      *CheckedPackage
}

// newLRUCache returns an empty lruCache holding at most capacity entries.
func newLRUCache(capacity int) *lruCache {
	return &lruCache{capacity: capacity, order: list.New(), index: make(map[string]*list.Element, capacity)}
}

// get returns pkgPath's cached CheckedPackage, if present, moving it to the
// front (most recently used).
func (l *lruCache) get(pkgPath string) (*CheckedPackage, bool) {
	el, ok := l.index[pkgPath]
	if !ok {
		return nil, false
	}
	l.order.MoveToFront(el)
	entry, _ := el.Value.(*lruEntry)
	return entry.cp, true
}

// put inserts or replaces pkgPath's cached CheckedPackage, evicting the
// least recently used entry first if the cache is already at capacity.
func (l *lruCache) put(pkgPath string, cp *CheckedPackage) {
	if el, ok := l.index[pkgPath]; ok {
		entry, _ := el.Value.(*lruEntry)
		entry.cp = cp
		l.order.MoveToFront(el)
		return
	}
	if l.order.Len() >= l.capacity {
		l.evictOldest()
	}
	el := l.order.PushFront(&lruEntry{pkgPath: pkgPath, cp: cp})
	l.index[pkgPath] = el
}

// evictOldest removes the least recently used entry, if any.
func (l *lruCache) evictOldest() {
	back := l.order.Back()
	if back == nil {
		return
	}
	entry, _ := back.Value.(*lruEntry)
	delete(l.index, entry.pkgPath)
	l.order.Remove(back)
}

// len returns the number of entries currently cached.
func (l *lruCache) len() int { return l.order.Len() }
