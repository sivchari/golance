package index

import (
	"encoding/binary"
	"hash/fnv"
	"sort"
	"sync"

	"github.com/sivchari/golance/internal/store"
)

// depExportEntry is one direct workspace dependency's contribution to a
// package's [computeUnitKey]: its import path and its current export data
// hash (see [store.UnitPointer].ExportHash's doc for why export hash, not
// content hash or blob key, is what must be folded in here).
type depExportEntry struct {
	path       string
	exportHash uint64
}

// computeUnitKey returns the CAS blob key for a package whose own source
// content hashes to ownContentHash and whose direct workspace dependencies'
// current export data hashes are deps.
//
// This is the redesign's correctness core (see plan-feat-v0.1.md): folding
// each dependency's *export* hash — not its content hash, and not its own
// blob key — into the key means:
//
//   - A body-only edit to a dependency (its ContentHash changes, its
//     exported API and so its ExportHash does not) never changes this
//     package's key, so it is never forced through revalidation just
//     because something it depends on was merely re-type-checked. This
//     preserves the existing, tested propagation-cutoff behavior
//     (internal/index's Reindex has always compared export data bytes for
//     exactly this reason; Build now shares the same soundness property for
//     free, closing a gap the pre-redesign Build had against a full
//     rebuild that skipped by content hash alone without considering
//     dependents at all).
//   - An API-changing edit to a dependency (its ExportHash changes) always
//     changes every direct importer's key, forcing it back through
//     revalidation — a CAS hit if this exact (own content, dependency API)
//     combination was already built before (e.g. switching back to a
//     previously-visited branch costs nothing beyond the key comparison
//     itself), or a real re-type-check otherwise.
//
// Only direct *workspace* (root) dependencies are folded in: an external
// module or stdlib dependency's identity is already fully determined by
// go.mod/go.sum, a change to which triggers a fresh graph load
// (internal/server's revalidateGraph) independent of this key — folding
// those in as well is unaddressed scope for this redesign, unchanged from
// the pre-redesign behavior.
func computeUnitKey(ownContentHash uint64, deps []depExportEntry) uint64 {
	sorted := append([]depExportEntry(nil), deps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].path < sorted[j].path })

	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], ownContentHash)
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte{0})
	for _, d := range sorted {
		_, _ = h.Write([]byte(d.path))
		_, _ = h.Write([]byte{0})
		binary.LittleEndian.PutUint64(buf[:], d.exportHash)
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// hashExport returns a deterministic hash of export data bytes, for
// [store.UnitPointer].ExportHash.
func hashExport(export []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(export)
	return h.Sum64()
}

// unitKeyRecord is what [keyTable] tracks per package: just enough of its
// current [store.UnitPointer] for a dependent to fold into its own
// [computeUnitKey] call, without needing to read anything beyond this
// database's small per-root index (see the package doc).
type unitKeyRecord struct {
	blobKey    uint64
	exportHash uint64
}

// keyTable resolves each root package's current unitKeyRecord, memoizing
// lookups across one Build or Reindex run: a package processed earlier in
// this same (topologically-ordered) run has its freshly computed record
// available via set; a package not touched this run falls back to whatever
// db already has recorded for it, since dependency-order processing
// guarantees any record this run needs has already stabilized — either
// updated moments ago by this same run, or, for a package this run leaves
// entirely untouched, unaffected and so still exactly what db already says.
type keyTable struct {
	db *store.DB
	mu sync.Mutex
	m  map[string]unitKeyRecord
}

func newKeyTable(db *store.DB) *keyTable {
	return &keyTable{db: db, m: make(map[string]unitKeyRecord)}
}

func (t *keyTable) set(path string, rec unitKeyRecord) {
	t.mu.Lock()
	t.m[path] = rec
	t.mu.Unlock()
}

// get returns path's current unitKeyRecord. ok is false only if path has
// never been indexed at all (no [store.UnitPointer] recorded and nothing
// computed for it yet this run) — e.g. a dependency that itself has no Go
// files, which is never scheduled as a job of its own (see
// internal/index's Build/Reindex; the caller filters these out before ever
// calling get for them).
func (t *keyTable) get(path string) (unitKeyRecord, bool) {
	t.mu.Lock()
	rec, ok := t.m[path]
	t.mu.Unlock()
	if ok {
		return rec, true
	}
	p, err := t.db.GetUnit(store.Hash(path))
	if err != nil {
		return unitKeyRecord{}, false
	}
	rec = unitKeyRecord{blobKey: p.BlobKey, exportHash: p.ExportHash}
	t.set(path, rec)
	return rec, true
}
