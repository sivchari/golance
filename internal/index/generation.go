package index

import (
	"sync"

	"github.com/sivchari/golance/internal/store"
)

// genTable orders the writes multiple concurrent Reindex calls against the
// same db may attempt for the same package, using the same
// assign-at-start/compare-at-commit shape internal/check.Engine's
// commit/commitCache use to solve the identical ordering problem for
// on-demand rechecks (see its doc): each Reindex call is handed one
// generation number via nextGen before it does any work, and every write it
// attempts for a package is gated by tryCommit against that package's own
// highest committed generation so far. Two Reindex calls have no ordering
// guarantee — a slower call started before a faster one can finish after it
// — so a call's write for a package it shares with a later-generation call
// that already committed one is silently dropped instead of overwriting it:
// a write here never replaces a newer one.
type genTable struct {
	mu   sync.Mutex
	next uint64
	done map[uint64]uint64 // pkgHash -> highest generation committed for it
}

// generationTables holds one genTable per *store.DB this process has run
// Reindex against, so concurrent Reindex calls sharing the same db — every
// real deployment dispatches one background reindex per didSave through the
// same long-lived idx.db handle — also share the ordering state that
// protects them from each other. A db is never removed from this map, so
// its size is bounded by the number of index databases opened in one
// process, not by the number of Reindex calls against them.
var generationTables sync.Map // *store.DB -> *genTable

// genTableFor returns db's genTable, creating and storing one if this is
// db's first Reindex call in this process.
func genTableFor(db *store.DB) *genTable {
	v, _ := generationTables.LoadOrStore(db, &genTable{done: make(map[uint64]uint64)})
	gt, _ := v.(*genTable)
	return gt
}

// nextGen assigns and returns the next monotonic generation number for a
// new Reindex call against g's db, to hand to every write that call
// attempts (see tryCommit).
func (g *genTable) nextGen() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return g.next
}

// tryCommit reports whether gen is still current for pkgHash — no
// higher-generation write has already been committed for it — recording
// gen as pkgHash's new high-water mark if so. A caller must not perform its
// write when this returns false.
func (g *genTable) tryCommit(pkgHash, gen uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gen < g.done[pkgHash] {
		return false
	}
	g.done[pkgHash] = gen
	return true
}
