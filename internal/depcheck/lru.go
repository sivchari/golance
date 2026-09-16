package depcheck

import (
	"container/list"
	"go/types"
)

// lruCache is a fixed-capacity, least-recently-used cache of
// *CheckedPackage values keyed by import path. Not safe for concurrent use
// on its own — Provider serializes every call to it under its own mutex.
//
// capacity is a target, not a hard ceiling: put pins every entry's own
// direct dependencies (cp.Types().Imports(), one level — chaining across
// entries covers the full transitive closure, see put's own doc) for as
// long as that entry itself stays cached, and evictOldest never removes a
// still-pinned entry regardless of capacity pressure. The cache can
// therefore temporarily hold more than capacity entries — bounded by the
// union of every currently-referenced closure (the concurrently in-flight
// or still-cached CheckedPackage set), not by workspace size, the same
// bounding argument DefaultCap/RecommendedCap's own docs already make for
// "how much concurrent work needs to stay resident," now extended to cover
// dependencies a cached entry (not just an in-flight one) still needs.
type lruCache struct {
	capacity int
	order    *list.List // MRU at Front, LRU at Back; element.Value is *lruEntry
	index    map[string]*list.Element
	pins     map[string]int32 // pkgPath -> number of OTHER cached entries whose own deps include it; see put's own doc
	pending  map[string]bool  // pkgPath -> a delete arrived while pinned; applied once the last pin releases
}

type lruEntry struct {
	pkgPath string
	cp      *CheckedPackage
	deps    []string // the paths THIS entry's own put pinned on its behalf (cp.Types().Imports()); released when this entry is itself removed
}

// newLRUCache returns an empty lruCache holding at most capacity entries
// under ordinary conditions (see lruCache's own doc for why this is a
// target, not a hard ceiling).
func newLRUCache(capacity int) *lruCache {
	return &lruCache{
		capacity: capacity, order: list.New(), index: make(map[string]*list.Element, capacity),
		pins: make(map[string]int32), pending: make(map[string]bool),
	}
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

// put inserts or replaces pkgPath's cached CheckedPackage, pinning
// cp.Types().Imports() — pkgPath's own direct dependencies — for as long as
// pkgPath itself stays cached (released by removeElement when pkgPath is
// finally removed, not by any particular check's own lifetime), evicting
// the least recently used UNPINNED entry first if the cache is already at
// capacity (see evictOldest).
//
// This is the cross-call counterpart to closureScope's own in-flight
// protection (#116): two DIFFERENT top-level Provider.Package calls,
// neither one still active, can each reach the same shared dependency
// through two DIFFERENT already-cached packages whose own entries must not
// be evicted out from under either of them once nothing is actively
// resolving them anymore — the mechanism internal/depexport's own
// checkAndPersist doc names and TestCache_UndersizedCapNeverReturnsAnUndecodableBlob
// reproduces (a widely-shared dependency gets evicted and re-checked from
// scratch between two SEPARATE closures reaching it, producing a distinct
// *types.Package for declaration-for-declaration identical source, which
// go/types then treats as non-identical for generic instantiation/
// interface-satisfaction purposes). Pinning only ONE level (cp's own direct
// Imports(), not its full transitive closure) is sufficient: a package
// survives exactly as long as anything that transitively depends on it
// does, via chaining — pkgPath pins its own deps, and those deps, for as
// long as THEY stay cached, pin THEIR OWN deps in turn — without pkgPath
// ever needing to compute or store its own full transitive closure.
//
// Eviction therefore naturally proceeds in reverse-dependency order over
// time: a widely-depended-upon entry always has pins > 0 for as long as
// anything that reaches it is still cached, so evictOldest can only ever
// remove entries nothing (no longer) needs, never one still-cached code
// still depends on.
func (l *lruCache) put(pkgPath string, cp *CheckedPackage) {
	deps := importPaths(cp.Types().Imports())
	if el, ok := l.index[pkgPath]; ok {
		entry, _ := el.Value.(*lruEntry)
		l.unpinAll(entry.deps)
		entry.cp = cp
		entry.deps = deps
		l.pinAll(deps)
		l.order.MoveToFront(el)
		return
	}
	if l.order.Len() >= l.capacity {
		l.evictOldest()
	}
	el := l.order.PushFront(&lruEntry{pkgPath: pkgPath, cp: cp, deps: deps})
	l.index[pkgPath] = el
	l.pinAll(deps)
}

// importPaths returns each imported package's own import path. "unsafe"
// (types.Unsafe) is skipped: it is never itself a cached entry (see
// Provider.unsafePackage), so pinning it would only ever be a wasted,
// never-consulted counter.
func importPaths(imports []*types.Package) []string {
	paths := make([]string, 0, len(imports))
	for _, imp := range imports {
		if imp == types.Unsafe {
			continue
		}
		paths = append(paths, imp.Path())
	}
	return paths
}

// pin increments pkgPath's pin count by one — an in-flight closureScope's
// own protection (see Provider.get/put's own wiring), contributing to the
// identical refcount put's own dependency-based pins do (see put's own
// doc): an entry survives as long as either source still needs it.
func (l *lruCache) pin(pkgPath string) {
	l.pins[pkgPath]++
}

// pinAll increments each path's pin count by one.
func (l *lruCache) pinAll(paths []string) {
	for _, p := range paths {
		l.pins[p]++
	}
}

// unpinAll releases one pin on each path (see unpin).
func (l *lruCache) unpinAll(paths []string) {
	for _, p := range paths {
		l.unpin(p)
	}
}

// unpin releases one pin on pkgPath, applying a pending delete (see
// l.pending) once pkgPath's last pin releases — which may itself cascade
// into removing whatever THAT removal was itself the last pin on.
func (l *lruCache) unpin(pkgPath string) {
	if l.pins[pkgPath] > 1 {
		l.pins[pkgPath]--
		return
	}
	delete(l.pins, pkgPath)
	if l.pending[pkgPath] {
		delete(l.pending, pkgPath)
		l.removeIfPresent(pkgPath)
	}
}

// evictOldest removes the least recently used entry that is NOT currently
// pinned (see put's own doc), walking from the LRU tail toward the front
// until it finds one. If every entry is pinned, this is a no-op — the
// cache temporarily exceeds capacity rather than evicting something a
// still-cached entry depends on (see lruCache's own doc on why that bound
// still holds).
func (l *lruCache) evictOldest() {
	for el := l.order.Back(); el != nil; el = el.Prev() {
		entry, _ := el.Value.(*lruEntry)
		if l.pins[entry.pkgPath] > 0 {
			continue
		}
		l.removeElement(el)
		return
	}
}

// removeElement removes el from order/index and releases every pin its own
// entry placed on its own dependencies (see put's own doc), which may in
// turn make one of those paths eligible for its own pending delete via
// unpin's cascade.
func (l *lruCache) removeElement(el *list.Element) {
	entry, _ := el.Value.(*lruEntry)
	delete(l.index, entry.pkgPath)
	l.order.Remove(el)
	l.unpinAll(entry.deps)
}

// removeIfPresent removes pkgPath's entry unconditionally (see
// removeElement), a no-op if absent.
func (l *lruCache) removeIfPresent(pkgPath string) {
	el, ok := l.index[pkgPath]
	if !ok {
		return
	}
	l.removeElement(el)
}

// delete removes pkgPath's cached entry, if present, deferring the removal
// until every pin on it releases if it is currently pinned (see
// Provider.Delete's own doc for why: some OTHER still-cached entry may
// still depend on it).
func (l *lruCache) delete(pkgPath string) {
	if l.pins[pkgPath] > 0 {
		l.pending[pkgPath] = true
		return
	}
	l.removeIfPresent(pkgPath)
}

// len returns the number of entries currently cached.
func (l *lruCache) len() int { return l.order.Len() }
