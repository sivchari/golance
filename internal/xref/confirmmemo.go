package xref

import (
	"container/list"
	"sync"
)

// confirmMemoCapacity bounds each confirmMemo instance by entry count
// rather than bytes: unlike r.units/r.cache (which hold decoded blob bytes
// or *types.Package object graphs, genuinely large per entry), a
// confirmMemo entry is a small symbol list -- a handful of candidateKey/
// resolvedSymbol values, not source bytes or a type graph -- so a byte
// accounting would be spurious precision for what is, per entry, a
// negligible allocation. A few thousand entries comfortably covers every
// distinct interface/concrete-type identity a real session's worth of
// Implementation/References queries would touch.
const confirmMemoCapacity = 4096

// confirmMemo is a bounded, LRU-evicted, mutex-guarded memo of one
// confirmation function's result per key K, shared across every query for a
// Resolver's whole lifetime -- the same staleness story as r.units/
// r.cache/exportCache (see their docs): a memoized entry cannot outlive its
// Resolver, since internal/server's indexState installs a brand-new
// Resolver (and so a brand-new, empty confirmMemo) on every reindex, and
// Resolver.Invalidate additionally clears every confirmMemo outright on a
// didSave-triggered partial reindex within the SAME Resolver's lifetime
// (see Invalidate's own doc for why a full clear, not a surgical
// per-package drop).
//
// Mirrors unitCache's own list+map LRU shape (see its doc), parameterized
// over key/value instead of duplicating that bookkeeping per confirmation
// function -- implementingTypes, embeddingInterfaces, implementedInterfaces,
// and interfacesSatisfiedByMethod each get their own confirmMemo instance
// (different K/V shapes, so different cache tables; see implementation.go's
// wrappers) rather than sharing one map keyed by some encoded union type.
type confirmMemo[K comparable, V any] struct {
	mu    sync.Mutex
	order *list.List // MRU at Front, LRU at Back; element.Value is *confirmMemoEntry[K, V]
	index map[K]*list.Element
	cap   int
}

type confirmMemoEntry[K comparable, V any] struct {
	key   K
	value V
}

// newConfirmMemo returns an empty confirmMemo bounded to capacity entries.
func newConfirmMemo[K comparable, V any](capacity int) *confirmMemo[K, V] {
	return &confirmMemo[K, V]{order: list.New(), index: make(map[K]*list.Element), cap: capacity}
}

// get returns key's memoized value, if present, moving it to the front
// (most recently used).
func (m *confirmMemo[K, V]) get(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.index[key]
	if !ok {
		var zero V
		return zero, false
	}
	m.order.MoveToFront(el)
	entry, _ := el.Value.(*confirmMemoEntry[K, V])
	return entry.value, true
}

// put inserts key's confirmed value, evicting the least-recently-used entry
// first if m is already at capacity. No-op if key is already present (a
// concurrent caller memoized the identical, deterministic result first;
// see implementation.go's wrappers for why re-running the confirmation is
// harmless even without singleflight collapsing it) beyond refreshing its
// recency.
func (m *confirmMemo[K, V]) put(key K, value V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.index[key]; ok {
		m.order.MoveToFront(el)
		return
	}
	el := m.order.PushFront(&confirmMemoEntry[K, V]{key: key, value: value})
	m.index[key] = el
	for m.order.Len() > m.cap {
		back := m.order.Back()
		if back == nil {
			break
		}
		entry, _ := back.Value.(*confirmMemoEntry[K, V])
		delete(m.index, entry.key)
		m.order.Remove(back)
	}
}

// clear discards every memoized entry, for Resolver.Invalidate (see its
// doc).
func (m *confirmMemo[K, V]) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.order = list.New()
	m.index = make(map[K]*list.Element)
}

// len returns the number of entries currently memoized, for test
// observability (asserting confirmMemoCapacity eviction at fixture scale).
func (m *confirmMemo[K, V]) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.index)
}
