package server

import (
	"context"
	"go/types"

	"github.com/sivchari/golance/internal/check"
)

// engineImporter adapts depCacheHolder's own dependency importer into a
// check.Importer for check.Engine's own dependency resolution: while the
// facts index is ready (see importer's own doc for the cold-build
// exception), every import it resolves for a ROOT (workspace) package tries
// two tiers, in order, before ever falling through to depCache's ordinary
// export-data path (which has no persistent cache at all for a root
// package — internal/depexport.Cache only ever persists GOROOT/GOMODCACHE
// directories — and so would re-source-check that package's whole
// transitive closure on every single query, the cause of a warm
// cross-package hover/definition hang the first of engineImporter's two
// tiers below already fixed once, and its own cold-cache-entry latency this
// second tier now also addresses):
//
//  1. decodeRoot: pkgPath's export data straight from the facts index's own
//     already-persisted blob (rootExportSource), whenever that blob is
//     still known current — a single CAS read plus a gcexportdata decode,
//     with no type-checking at all. This is the common case for a root
//     package the SERVER itself has not edited (or reindexed) since the
//     index last built it: everything the indexer subprocess already
//     checked once, during the cold build, is reused here instead of
//     rechecked.
//  2. fallback's own Get/commit cache (check.Engine.GetPackage), for
//     whatever decodeRoot could not answer (no index yet, an open/dirty
//     file, or a package the index's own content-hash comparison reports
//     stale) — a full, overlay-aware, content-hash-validated recheck of
//     THAT ONE package, its own imports resolved through this identical
//     importer (so a further-stale import recurses into fallback again,
//     never into a full recursive re-check of the whole closure). fallback
//     is a SEPARATE, small check.Engine from the one this importer itself
//     feeds — never that Engine's own cache — precisely so this tier's cost
//     is bounded independent of workspace size instead of scaling engine's
//     cache to the whole root-package closure; see setWorkspace's own
//     construction of it for the full reasoning and the regression that
//     drove the split.
//
// fallback is set once, right after check.New returns the Engine it names —
// a two-phase construction (check.New needs this importer's method value
// before the Engine it will call it against exists) resolved the same way
// setWorkspace resolves the rest of its own construction: check.New never
// calls the importer synchronously during construction, only later during
// an actual recheck, so setting fallback afterward, before any concurrent
// use, is safe. See setWorkspace for the exact construction order.
type engineImporter struct {
	depCache   *depCacheHolder
	graphSrc   *check.GraphSource
	rootExport *rootExportSource
	fallback   *check.Engine
}

// importer implements check.Importer. It only wraps depCache's own importer
// with rootAwareImporter while the facts index is ready (ei.depCache.
// indexReady()): during a cold index build, a root package's import stays
// unresolved for the whole build window instead, exactly as it did before
// this type existed — see depCacheHolder.importer's own cold-index-build
// gate doc for why (bounding the server's own memory while the indexer
// subprocess is already checking the same packages). Resolving it eagerly
// through fallback.GetPackage (or decodeRoot, which itself defers to the
// same gate — see rootExportSource's own doc) during that window would
// reintroduce a form of the same problem: every root package transitively
// reachable from whatever file happens to be open would get type-checked by
// the server's own Engine concurrently with the indexer doing the same
// work.
func (ei *engineImporter) importer(ctx context.Context) types.ImporterFrom {
	base := ei.depCache.importer()
	if !ei.depCache.indexReady() {
		return base
	}
	return rootAwareImporter{ctx: ctx, base: base, isRoot: ei.graphSrc.IsRoot, getRoot: ei.fallback.GetPackage, decodeRoot: ei.decodeRoot}
}

// decodeRoot resolves pkgPath's already-persisted export data from the
// facts index (ei.rootExport.blob), decoding it into ei.depCache's own
// current (fset, cache) pair — the exact same pair base (ei.depCache.
// importer's return value) resolves every non-root import in this same
// recheck against, so a package a root import re-exports and a non-root
// import also reaches keeps a single, consistent *types.Package identity
// regardless of which tier resolved it first. ok is false whenever
// rootExport.blob itself reports ok=false, or the blob it did return fails
// to decode (e.g. a schema mismatch from an index built by an older
// golance) — either way the caller falls back to a full check via
// fallback.GetPackage, never to an error.
func (ei *engineImporter) decodeRoot(ctx context.Context, pkgPath string) (*types.Package, bool) {
	data, ok := ei.rootExport.blob(ctx, pkgPath)
	if !ok {
		return nil, false
	}
	pkg, err := ei.depCache.decodeExport(pkgPath, data)
	if err != nil {
		return nil, false
	}
	return pkg, true
}

// rootAwareImporter is a types.ImporterFrom that resolves a ROOT package
// import through decodeRoot (the facts index's own persisted export data,
// see engineImporter's own doc) first, then getRoot (fallback's own small,
// SEPARATE check.Engine cache, i.e. check.Engine.GetPackage on the
// dedicated Engine engineImporter.fallback names — never the Engine this
// importer itself feeds, see engineImporter's own doc for why) for whatever
// decodeRoot could not answer, before ever falling through to base — the
// ordinary export-data-decoding importer depCacheHolder produces, which for
// a ROOT pkgPath answers only via depCache's non-persistent depexport.Cache
// fallback — the pre-fix cost this two-tier ordering exists to avoid. A
// path neither decodeRoot nor getRoot can resolve (e.g. ctx canceled, or
// the package is not one fallback's own SnapshotSource knows by import
// path) falls back to base rather than failing outright: base's own
// depexport-backed resolution still answers a root pkgPath correctly, just
// at the pre-fix cost, so this never introduces a new failure mode, only a
// slower fallback for a case neither faster tier could already serve.
//
// Import identity across a single type-check is preserved regardless of
// which of the three tiers resolves a given root import: decodeRoot decodes
// into depCache's own (fset, cache) pair, the identical pair base itself
// resolves every non-root import against (see engineImporter.decodeRoot's
// own doc — gcexportdata's shared imports map keeps a package referenced
// from two different decodes, in either order, pointer-identical); getRoot's
// own content-hash cache (fallback's, entirely independent of depCache's and
// of the Engine this importer feeds) always returns the same *CheckedPackage
// (and so the same *types.Package) for a given unit until its content
// changes, so two packages importing the same root dependency — within the
// same recheck or a different one — see pointer-identical *types.Package
// values there too, exactly as go/types requires WITHIN one recheck's own
// resolution (identity is not, and need not be, shared between a package
// resolved here via fallback and that same package later resolved directly
// by engine's own Get, e.g. once the user opens its file — those are
// separate checks against separate Engines, and go/types never compares
// object identity across two unrelated Check calls).
//
// Recursion safety: getRoot recursively calls back into fallback while this
// importer is itself running inside another recheck — on either engine or
// fallback itself, since both share this exact importer (see setWorkspace's
// own construction). That recursive call only ever joins or starts a flight
// for the imported package's own unit — never the caller's — and never
// holds any lock across its own blocking wait (see check.Engine.GetPackage's
// doc), so it cannot deadlock against the recheck that triggered it. Go
// itself forbids import cycles, so the package graph reachable this way is
// always a DAG: a recursive getRoot call can never, even transitively, need
// to resolve back to the unit that started it. decodeRoot carries no such
// recursion at all — it never calls back into an Engine, only into the
// facts index and depCache's own decode cache.
type rootAwareImporter struct {
	ctx        context.Context
	base       types.ImporterFrom
	isRoot     func(pkgPath string) bool
	getRoot    func(ctx context.Context, pkgPath string) (*check.CheckedPackage, error)
	decodeRoot func(ctx context.Context, pkgPath string) (*types.Package, bool)
}

// Import implements types.Importer.
func (r rootAwareImporter) Import(path string) (*types.Package, error) {
	return r.ImportFrom(path, "", 0)
}

// ImportFrom implements types.ImporterFrom.
func (r rootAwareImporter) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if r.isRoot(path) {
		if pkg, ok := r.decodeRoot(r.ctx, path); ok {
			return pkg, nil
		}
		if cp, err := r.getRoot(r.ctx, path); err == nil {
			return cp.Package(), nil
		}
	}
	return r.base.ImportFrom(path, dir, mode)
}
