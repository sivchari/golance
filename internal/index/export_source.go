package index

import (
	"context"
	"sync"

	"github.com/sivchari/golance/internal/store"
)

// memExportSource holds self-authored export data blobs produced during a
// single Build or Reindex run, keyed by import path, satisfying
// typecheck.ExportSource. It never evicts entries: unlike decoded
// *types.Package values, raw export bytes are small enough to keep for a
// whole run, and any not-yet-processed package may need any earlier one's
// blob regardless of how long ago it was produced.
type memExportSource struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newMemExportSource() *memExportSource {
	return &memExportSource{blobs: make(map[string][]byte)}
}

// Put records blob as pkgPath's export data.
func (s *memExportSource) Put(pkgPath string, blob []byte) {
	s.mu.Lock()
	s.blobs[pkgPath] = blob
	s.mu.Unlock()
}

// ExportData implements typecheck.ExportSource.
func (s *memExportSource) ExportData(pkgPath string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, ok := s.blobs[pkgPath]
	return blob, ok, nil
}

// casExportSource resolves export data for a Build or Reindex run's shared
// typecheck.Importer: an eagerly-populated entry (a package freshly hit or
// type-checked earlier in this same run, via Put) takes priority; otherwise
// it looks up the requesting package's current CAS blob key through keys
// and, only then, lazily loads that blob from cas to read its export data.
//
// This laziness is what makes an untouched run (nothing changed anywhere)
// cost zero CAS reads: a chain of skipped packages never needs its export
// data at all, since nothing is re-type-checking against it — the goal
// being a no-op restart with no changes on disk.
type casExportSource struct {
	ctx  context.Context
	mem  *memExportSource
	cas  *store.CAS
	keys *keyTable
}

// newCASExportSource returns a casExportSource for one Build or Reindex
// run, checked against ctx (that run's own context) on every CAS read (see
// ExportData) — stored once here rather than threaded through ExportData
// itself, since it implements typecheck.ExportSource, an interface go/types'
// own importer machinery calls with no context parameter of its own.
func newCASExportSource(ctx context.Context, cas *store.CAS, keys *keyTable) *casExportSource {
	return &casExportSource{ctx: ctx, mem: newMemExportSource(), cas: cas, keys: keys}
}

// Put records blob as pkgPath's export data, taking priority over a lazy
// CAS load for the rest of this run (see the type doc).
func (s *casExportSource) Put(pkgPath string, blob []byte) {
	s.mem.Put(pkgPath, blob)
}

// ExportData implements typecheck.ExportSource. An empty blob — Put with
// nil/empty bytes, or a CAS-persisted UnitBlob whose Export is nil/empty —
// is reported as a miss (ok=false), not a hit with empty data: both cases
// mean checkAndStoreOutcome withheld pkgPath's export because it failed
// writeAndValidateExport's round-trip check (see checkOnePackage's own
// doc), not that pkgPath genuinely has no exported API (a real, exported-
// nothing package's blob is never actually empty — gcexportdata always
// writes a non-empty header). Reporting ok=true here would make
// Importer.resolve decode zero bytes and fail outright for every
// dependent, instead of falling through to imp.fallback — the
// declaration-only source-check path (internal/depexport.Cache, backed by
// depcheck.Provider, which source-checks a workspace/root package exactly
// as readily as a stdlib/module one — see depcheck.GraphMetadataSource's
// doc) — that can still give a dependent a complete, usable answer despite
// pkgPath's own compiled export being unusable.
func (s *casExportSource) ExportData(pkgPath string) ([]byte, bool, error) {
	if blob, ok, _ := s.mem.ExportData(pkgPath); ok {
		if len(blob) == 0 {
			return nil, false, nil
		}
		return blob, true, nil
	}
	rec, ok := s.keys.get(pkgPath)
	if !ok {
		return nil, false, nil
	}
	blob, ok, err := s.cas.Get(s.ctx, rec.blobKey)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		return nil, false, err
	}
	if len(u.Export) == 0 {
		return nil, false, nil
	}
	return u.Export, true, nil
}
