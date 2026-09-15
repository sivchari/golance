package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"go/token"
	"go/types"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/depexport"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/typecheck"
)

// maxDepCacheBytes bounds depCache's decode cache, measured by
// typecheck.Cache's own naive byte estimate (the sum of serialized
// export-data blob sizes, see typecheck.Cache.Bytes) before it is discarded
// and replaced with an empty one. v0.1 has no precise heap accounting for
// decoded *types.Package values, so this is deliberately a coarse,
// whole-cache eviction rather than a per-package LRU.
//
// This must stay a generous, blob-byte cap: scaling the comparison up to
// approximate the larger live *types.Package heap those blobs decode into
// (as an earlier revision briefly did) shrinks the effective threshold to a
// fraction of what maxDepCacheBytes's own name promises, evicting mid-check
// and forcing every subsequent recheck to redecode a dependency closure that
// would otherwise stay resident — see exportDecodeCap's identical tradeoff
// in internal/depcheck/export.go, whose own doc has the full reasoning
// (this holder's eviction only ever fires between checks, at importer()'s
// own call boundary, so it cannot corrupt a single check's own in-flight
// resolution the way exportResolver's per-decode eviction could; it is
// still wasteful thrashing to shrink the cap here for the same reason). The
// cold-start server-RSS blowup that motivated scaling by a decoded-heap
// multiplier in the first place is bounded by the cold-index gate (see
// importer's coldGateSource), not by shrinking this cache's cap.
// A var, not a const, purely as a test seam: TestDepCacheHolder_Importer_
// EvictsWholeCacheOncePastByteCap (workspace_test.go) lowers it for the
// length of one test rather than generating fixture export data anywhere
// near the real 512MiB default, which no production code path ever
// mutates.
var maxDepCacheBytes int64 = 512 * 1024 * 1024 // 512MiB blob-byte cap

// depCacheHolder owns the persistent typecheck.Cache the check engine's
// dependency importer decodes into across many rechecks, plus the single
// *token.FileSet it is tied to (see typecheck.Cache's doc: a Cache and the
// fset it was decoded into must be discarded together, never
// independently). exports resolves external-module and stdlib export data
// by declaration-only source-checking it (internal/depexport.Cache — see
// its package doc for what replaced typecheck.ExportFileSource/`go list
// -export`); those are treated as immutable for the life of a workspace,
// since any change to them implies a go.mod/go.sum change, which already
// triggers a full setWorkspace (and so a fresh depCacheHolder) via
// revalidateGraph. provider and exportProvider are this same workspace's
// two depcheck.Provider instances (see ensureDepProvider's doc for why
// export-data production needs its OWN, decode-disabled Provider rather
// than sharing depProvider's), kept here only so invalidate can drop a
// changed package from both (see its own doc) — neither is otherwise used
// for resolution here, exports already wraps exportProvider for that.
type depCacheHolder struct {
	exports        typecheck.ExportSource
	provider       *depcheck.Provider
	exportProvider *depcheck.Provider
	// indexReady reports whether the facts index is currently open — false
	// for the whole span of a cold index build (see importer's own doc for
	// why this gates which ExportSource tier importer resolves through).
	// Always non-nil in production (setWorkspace supplies s.idx.Load()!=nil);
	// a nil value would panic on the first importer() call, exactly like any
	// other unset required constructor argument.
	indexReady func() bool

	mu    sync.Mutex
	fset  *token.FileSet
	cache *typecheck.Cache
}

func newDepCacheHolder(exports typecheck.ExportSource, provider, exportProvider *depcheck.Provider, indexReady func() bool) *depCacheHolder {
	return &depCacheHolder{
		exports: exports, provider: provider, exportProvider: exportProvider, indexReady: indexReady,
		fset: token.NewFileSet(), cache: typecheck.NewCache(),
	}
}

// coldExportSource is the additional capability internal/depexport.Cache
// offers beyond typecheck.ExportSource: resolving a pkgPath from ONLY
// already-persisted data, reporting ok=false rather than ever running an
// expensive, recursive from-source check when nothing is cached yet (see
// depexport.Cache.ExportDataFromCache's own doc). importer's cold-build gate
// type-asserts d.exports against this rather than requiring it structurally,
// the same optional-capability idiom internal/check.openChecker/dirLister
// already use: a depCacheHolder built in a test with a plain ExportSource
// stub that does not implement it simply never gates, resolving through
// exports directly regardless of indexReady — unchanged pre-gate behavior.
type coldExportSource interface {
	ExportDataFromCache(pkgPath string) (data []byte, ok bool, err error)
}

// coldGateSource adapts a coldExportSource into a typecheck.ExportSource
// that answers only from already-persisted data, standing in for the
// ordinary, possibly-source-checking exports value while a cold index build
// is in progress (see importer's own doc).
type coldGateSource struct{ src coldExportSource }

func (g coldGateSource) ExportData(pkgPath string) ([]byte, bool, error) {
	return g.src.ExportDataFromCache(pkgPath)
}

// importer returns a types.ImporterFrom decoding into d's current
// (fset, cache) pair, first swapping in a fresh, empty pair if the current
// one has grown past maxDepCacheBytes (see its own doc).
//
// While the facts index is not yet ready (d.indexReady() is false — a cold
// index build still running, the same window the indexer subprocess is
// already type-checking this workspace's entire dependency closure in),
// the returned importer resolves an uncached import through
// coldGateSource instead of d.exports directly: a CAS hit still answers
// immediately, but a miss reports "no data" rather than falling through to
// depexport.Cache.checkAndPersist's from-source check, which recursively
// re-type-checks pkgPath's whole transitive closure — duplicating, IN THE
// SERVER PROCESS, work the indexer subprocess already does, and the
// confirmed cause of a multi-GB server heap on a large monorepo's cold
// start (see this package's own cold-start RSS investigation notes). go/
// types tolerates an ImporterFrom reporting an import as unresolved: it
// records a "could not import" error for that identifier and keeps
// checking the rest of the file, so a package with an unresolved dependency
// during a cold build still gets a usable (if partial) CheckedPackage —
// same-package/local definitions are unaffected. Once d.indexReady()
// reports true, the very next importer() call (the next recheck) resolves
// every import fully again, exactly as before this gate existed.
func (d *depCacheHolder) importer() types.ImporterFrom {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache.Bytes() > maxDepCacheBytes {
		d.fset = token.NewFileSet()
		d.cache = typecheck.NewCache()
	}
	exports := d.exports
	if !d.indexReady() {
		if cold, ok := exports.(coldExportSource); ok {
			exports = coldGateSource{cold}
		}
	}
	return typecheck.NewImporter(d.fset, nil, exports, d.cache)
}

// FileSet returns the *token.FileSet dependency export data is currently
// decoded into (see importer). Its positions are only meaningful against a
// *types.Package decoded by the same (fset, cache) pair still current when
// the caller reads them: a concurrent recheck that pushes d past
// maxDepCacheBytes swaps in a fresh pair, after which an older decode's
// positions belong to neither the new fset this returns nor any fset the
// caller still has a reference to. That window is narrow in practice
// (512MiB of decoded export data) relative to how soon after a check a
// caller reads cp's objects, and accepted for the same reason importer's
// own eviction is.
func (d *depCacheHolder) FileSet() *token.FileSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fset
}

// decodeExport decodes data — pkgPath's already-persisted export data,
// resolved by a caller such as rootExportSource — into d's current
// (fset, cache) pair, via typecheck.ReadExport. Used by engineImporter.
// decodeRoot, whose own doc explains why this must land in d's pair rather
// than a separate one: it is the identical pair d's importer() return value
// resolves every non-root import in the very same recheck against, so a
// package reachable both ways keeps one consistent identity. Reads d.fset
// and d.cache under d.mu but calls typecheck.ReadExport outside it (that
// call takes cache's own internal lock instead): subject to the same
// narrow, accepted pair-swap race FileSet's own doc describes, since a
// concurrent recheck exceeding maxDepCacheBytes could swap in a fresh pair
// between this read and importer()'s own for the SAME recheck — the same
// tradeoff, not a new one.
func (d *depCacheHolder) decodeExport(pkgPath string, data []byte) (*types.Package, error) {
	d.mu.Lock()
	fset, cache := d.fset, d.cache
	d.mu.Unlock()
	return typecheck.ReadExport(data, fset, pkgPath, cache)
}

// invalidate drops pkgPaths from the current cache and from d.provider's and
// d.exportProvider's own LRUs (see depcheck.Provider.Delete's doc for why a
// workspace package can be cached there too — either Provider's recursive
// import resolution can reach one), so the next recheck that imports any of
// them re-decodes fresh export data instead of reusing a now-possibly-stale
// *types.Package. Callers use this after a workspace package's on-disk
// export data changes (didSave's background reindex).
func (d *depCacheHolder) invalidate(pkgPaths []string) {
	d.mu.Lock()
	cache := d.cache
	d.mu.Unlock()
	for _, p := range pkgPaths {
		cache.Delete(p)
	}
	d.provider.Delete(pkgPaths...)
	d.exportProvider.Delete(pkgPaths...)
}

// depMetadataSource is a depcheck.MetadataSource whose backing
// *graph.Snapshot can be swapped in place (see retarget). This is what lets
// a single, long-lived depcheck.Provider (see ensureDepProvider) keep
// answering dependency-package metadata lookups correctly across a
// setWorkspace snapshot swap, without needing a fresh Provider — and so
// without losing its type-check cache — every time: the Provider is built
// once against this wrapper, and only the wrapper's target snapshot moves.
// Safe for concurrent use: retarget and Package can race (a query in flight
// when a new snapshot lands), backed by an atomic.Pointer rather than a
// mutex since Package is on Provider's hot path.
type depMetadataSource struct {
	snap atomic.Pointer[graph.Snapshot]
}

// retarget points d at snap, so every subsequent Package call resolves
// against it instead of whatever snapshot was current before.
func (d *depMetadataSource) retarget(snap *graph.Snapshot) { d.snap.Store(snap) }

// Package implements depcheck.MetadataSource against d's current snapshot.
func (d *depMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	snap := d.snap.Load()
	if snap == nil {
		return "", nil, nil, false
	}
	pkg, ok := snap.Package(pkgPath)
	if !ok {
		return "", nil, nil, false
	}
	return pkg.Dir, pkg.GoFiles, pkg.Imports, true
}

// depsKey returns a stable digest of snap's non-workspace package set — the
// standard library and module-cache dependencies depcheck.Provider resolves
// (see workspace.depProvider's doc; a Root package, by contrast, is always
// routed to ws.engine instead — see nonWorkspacePackageForFile). Two
// snapshots produce the same key exactly when reusing a Provider built for
// one to serve the other cannot answer with stale content: a dependency
// version bump changes its resolved module-cache directory (Go's module
// cache paths are version-suffixed and immutable per version), and a GOROOT
// upgrade — the only way stdlib content itself could change — implies a
// process restart in practice, so this needs no separate go.mod/go.sum
// content hash, no filesystem I/O beyond what graph.Load already did, and
// no separate accounting for go.work's multi-module fan-out: whatever
// changed about the dependency set, it shows up here.
func depsKey(snap *graph.Snapshot) string {
	paths := make([]string, 0, len(snap.Packages))
	for path, pkg := range snap.Packages {
		if pkg.Root {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, path := range paths {
		pkg := snap.Packages[path]
		_, _ = h.Write([]byte(path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(pkg.Dir))
		_, _ = h.Write([]byte{0})
		files := append([]string(nil), pkg.GoFiles...)
		sort.Strings(files)
		for _, f := range files {
			_, _ = h.Write([]byte(f))
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte{'\n'})
	}
	return string(h.Sum(nil))
}

// ensureDepProvider returns the navigation depcheck.Provider, the export
// depcheck.Provider, and the depexport.Cache setWorkspace should install
// into the workspace it is building over snap: the server's current trio,
// retargeted at snap, if snap's dependency set (depsKey) matches the one
// they were built for — otherwise a fresh pair of Providers (and a fresh
// depMetadataSource shared by both) replacing them, with a fresh
// depexport.Cache built over the export Provider — s.depExportCAS itself is
// never rebuilt: it is the machine-global, cross-repository persistent
// store (see internal/depexport's package doc), opened once for this
// Server's whole lifetime, independent of any one workspace's dependency
// set.
//
// TWO separate Providers, not one shared instance (a prior revision of this
// method used one): depProviderVal has SetExportSource installed, so its
// own recursive import resolution (ctxImporter, inside check — walking a
// checked package's own transitive imports) can decode an already-persisted
// dependency's export data instead of a full recursive source-check — sound
// for depProviderVal's own consumers, since none of them (langfeat.
// DependencyDefinition navigation, and depCacheHolder's own compilation
// importer fallback) ever re-serializes the *types.Package it returns.
// depExportProviderVal never gets that call, since depExportsVal's own
// checkAndPersist DOES re-serialize its result via typecheck.WriteExport
// (gcexportdata.Write) to persist it.
//
// The actual corruption this guards against (confirmed by direct
// reproduction, independent of SetExportSource either way — see
// depexport.Cache.checkAndPersist's own doc for the mechanism and
// internal/depexport's TestCache_UndersizedCapNeverReturnsAnUndecodableBlob)
// is depcheck.Provider's own LRU eviction: a widely-shared dependency
// evicted and re-checked from scratch partway through one recursive check
// can leave the checked *types.Package referencing two non-identical
// instances for the same import path, a graph gcexportdata.Write accepts
// without error but gcexportdata.Read of those same bytes cannot reliably
// decode (observed: gcimporter panics with "internal error ... invalid
// memory address or nil pointer dereference", recovered into an opaque
// decode error). checkAndPersist's own round-trip self-check is what
// actually catches this — converting a would-be-corrupt blob into a clean,
// contained error instead of ever returning or persisting it — regardless
// of which Provider produced it. Keeping export production on its own
// Provider, separate from depProviderVal's decode-fast-path-enabled one, is
// an additional, narrower safety margin on top of that: it removes decode
// output — code this package does not otherwise need to reason about for
// correctness here — from the object graph checkAndPersist ever hands to
// WriteExport at all. The cost is depExportProviderVal not sharing
// depProviderVal's own warm cache for a dependency both navigation and
// export production need — reintroducing some of the LRU thrash
// depProviderVal's own decode fast path exists to avoid, but only for the
// export-production path, and strictly better than the corruption it
// guards against.
//
// Reuse is sound whenever the dependency set is unchanged: depProviderVal
// exists specifically to answer navigation into the standard library and
// module dependencies, content that does not change just because the user
// edited a workspace file, so keeping it — and its type-check cache — warm
// across a setWorkspace call driven by nothing but such an edit costs
// nothing in correctness. Rebuilding it — correctly, from scratch —
// whenever depsKey differs is what keeps that safe.
func (s *Server) ensureDepProvider(snap *graph.Snapshot) (navProvider, exportProvider *depcheck.Provider, exports *depexport.Cache) {
	key := depsKey(snap)

	s.depProviderMu.Lock()
	defer s.depProviderMu.Unlock()
	if s.depProviderVal == nil || key != s.depProviderKey {
		s.depProviderSrc = &depMetadataSource{}
		// Cap sized via depcheck.RecommendedCap, not depcheck.DefaultCap,
		// for BOTH Providers: depExportProviderVal can need to resolve a
		// workspace package's ENTIRE dependency closure to compile it (see
		// depCacheHolder's doc), same as depProviderVal's own worst case.
		// DefaultCap's small, navigation-sized capacity thrashes badly at
		// that scale; see depcheck.DefaultCap's own doc for the real,
		// measured regression this avoids. RecommendedCap itself bounds
		// worst-case memory to a size independent of workspace size (see
		// its own doc) rather than to snap's own package count.
		//
		// The count fed in is non-root only: a ROOT pkgPath reaches
		// depExportProviderVal via depexport.Cache.resolve's own
		// checkAndPersist(persist=false) path only in the rare case neither
		// decodeRoot nor rootFallback's own small cache could answer (see
		// rootAwareImporter's doc) — engineImporter's two faster tiers are
		// what keep that path rare, not this cap, so sizing it any larger
		// than depProviderVal's own non-root-only worst case would just cost
		// memory without fixing a real thrashing case in practice.
		providerCap := depcheck.RecommendedCap(nonRootPackageCount(snap), s.resolvedIndexJobs())
		s.depProviderVal = depcheck.NewProvider(s.depProviderSrc, depcheck.Options{Cap: providerCap})
		s.depExportProviderVal = depcheck.NewProvider(s.depProviderSrc, depcheck.Options{Cap: providerCap})
		s.depExportsVal = depexport.NewCache(s.depExportCAS, s.depProviderSrc, s.depExportProviderVal, depexport.Options{})
		// Lets depProviderVal's own recursive import resolution (ctxImporter,
		// walking a checked package's own transitive imports) consult
		// depExportsVal's persistent, machine-global CAS instead of always
		// re-parsing and re-type-checking a package evicted from its small
		// LRU mid-closure — see depcheck.Provider.SetExportSource's own doc
		// for the regression this fixes: a dependency closure larger than
		// the LRU's cap used to thrash it, turning "check every distinct
		// package once" into "recheck a widely-shared package once per
		// importer that reaches it again". depExportProviderVal deliberately
		// never gets this call — see this method's own doc.
		s.depProviderVal.SetExportSource(s.depExportsVal)
		s.depProviderKey = key
	}
	s.depProviderSrc.retarget(snap)
	return s.depProviderVal, s.depExportProviderVal, s.depExportsVal
}

// resolvedIndexJobs returns s.opts.IndexJobs, defaulted exactly like
// index.Options.Parallelism (index.Options.withDefaults' own identical
// formula) when it is <= 0 ("automatic") — the concurrency input
// ensureDepProvider's depcheck.RecommendedCap call sizes its cap from, for
// consistency with the indexer subprocess's own sizing even though this
// depProvider serves the live session, not a batch build.
func (s *Server) resolvedIndexJobs() int {
	if s.opts.IndexJobs > 0 {
		return s.opts.IndexJobs
	}
	return max(1, runtime.NumCPU()/2)
}

// nonRootPackageCount returns the number of non-root (stdlib/module-cache)
// packages in snap — the sizing input for depcheck.RecommendedCap (see
// ensureDepProvider's use).
func nonRootPackageCount(snap *graph.Snapshot) int {
	n := 0
	for _, pkg := range snap.Packages {
		if !pkg.Root {
			n++
		}
	}
	return n
}

// isExternalTestOfRoot reports whether pkg is a workspace directory's
// external "_test"-suffixed test package: pkg.ForTest names a Root package
// sharing pkg's own directory. Mirrors internal/index's identical
// predicate of the same name (kept as a separate copy per package, the same
// way this package's own ForTest exclusion already mirrors
// internal/check.GraphSource's) and internal/xref.Resolver's own copy —
// setWorkspace's fileToPkg build and nonWorkspacePackageForFile both use
// this to recognize such a file as still belonging to the workspace, not a
// GOROOT/module-cache dependency.
//
// The directory check excludes the rare intermediate-test-variant case
// documented on graph.Package.ForTest: a ForTest-tagged entry whose real
// files live in a completely different directory is not this directory's
// own test package at all.
func isExternalTestOfRoot(snap *graph.Snapshot, pkg *graph.Package) bool {
	if pkg.ForTest == "" {
		return false
	}
	base, ok := snap.Packages[pkg.ForTest]
	return ok && base.Root && base.Dir == pkg.Dir
}

// changedExportSet returns every package path in snap whose cached
// dependency export data (ws.depCache, keyed by import path) setWorkspace's
// reuse path can no longer trust, for depCache.invalidate to drop before
// the reused engine's next recheck of anything importing them. A package
// qualifies by having a different GoFiles set between old and snap — a file
// created or deleted in its directory, the only way needsGraphReload
// triggers a setWorkspace call without a go.mod/go.sum change (see
// setWorkspace's own doc for why a content-only edit is excluded) — or by
// having disappeared from snap entirely (removed from the workspace, e.g.
// its directory deleted). Each such package is expanded through the
// reverse-dependency closure of BOTH snap and old (Snapshot.ClosureUnits),
// each guarded by an existence check in that snapshot before the call:
// snap's closure catches every current importer of the change, and old's
// catches an importer snap no longer even has a node for (itself removed
// in the same reload, e.g. a package's only importer deleted alongside
// it) — either alone can miss an importer the other still knows about.
// Mirrors index.Reindex's own reverse-closure invalidation
// (internal/index/reindex.go's orderedReverseClosure), driven here off a
// GoFiles diff across two snapshots rather than off one caller-supplied
// changed package.
//
// A ForTest-tagged entry (graph.Package.ForTest != "") is skipped in both
// snapshots, the same exclusion setWorkspace's own fileToPkg/dirToPkg build
// and check.GraphSource.buildGraphIndex apply: it is a synthesized
// test-only node — most commonly an external "_test" package — that no
// real import path ever imports (see externalTestPkgPathMarker), so its
// own GoFiles changing implies nothing about any dependency importer's
// cached export data; the on-disk file change it reflects is already
// covered by the base package sharing its directory.
func changedExportSet(old, snap *graph.Snapshot) []string {
	changed := make(map[string]bool)
	for path, pkg := range snap.Packages {
		if pkg.ForTest != "" {
			continue
		}
		oldPkg, ok := old.Package(path)
		if !ok || !sameGoFiles(oldPkg.GoFiles, pkg.GoFiles) {
			changed[path] = true
		}
	}
	for path, pkg := range old.Packages {
		if pkg.ForTest != "" {
			continue
		}
		if _, ok := snap.Package(path); !ok {
			changed[path] = true
		}
	}

	set := make(map[string]bool, len(changed))
	for path := range changed {
		if _, ok := snap.Package(path); ok {
			for _, dep := range snap.ClosureUnits(path) {
				set[dep] = true
			}
		}
		if _, ok := old.Package(path); ok {
			for _, dep := range old.ClosureUnits(path) {
				set[dep] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// sameGoFiles reports whether a and b list the same GoFiles, order
// insensitive: go/packages makes no ordering guarantee across two separate
// Load calls over unchanged on-disk content, so a naive index comparison
// would report a spurious difference on every reload. In practice,
// graph.Load produces deterministic ordering, so an unchanged package's two
// listings compare equal elementwise without either allocating; the sorted
// comparison below only runs as a fallback for the rare package whose order
// did shift between loads.
func sameGoFiles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ordered := true
	for i := range a {
		if a[i] != b[i] {
			ordered = false
			break
		}
	}
	if ordered {
		return true
	}

	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// setWorkspace builds a workspace bundle over snap and installs it,
// replacing whatever workspace (if any) was loaded before. If a facts
// index is already open, its Resolver is rebuilt over the new snapshot too
// (the *store.DB itself is untouched — only the in-memory import-graph
// view a Resolver holds needs refreshing).
//
// When the outgoing workspace is being reloaded rather than replaced —
// same root, same dependency set (ensureDepProvider's own reuse decision,
// see reuse below) — ws.graphSrc, ws.engine, and ws.depCache are reused in
// place instead of rebuilt, because setWorkspace runs on every graph
// revalidation, not only at initialize: any go.mod/go.sum/go.work change,
// and any workspace/didChangeWatchedFiles batch that adds or removes a
// file in an already-known package directory (see needsGraphReload).
// Rebuilding engine from scratch on every such call would discard its
// whole per-unit check cache and cancel every in-flight Get flight running
// against it, so reuse is what keeps those warm across an ordinary edit.
//
// Reuse is sound for ws.engine's own cache without any extra bookkeeping:
// every cache entry is validated against a live, current on-disk/overlay
// content hash on every Get (see Engine.runRecheck's contentHash check),
// so an edit to a workspace package's own files always self-heals on its
// next check regardless of whether the graph snapshot backing the engine
// was ever refreshed. The one thing that check alone cannot catch is a
// workspace package's DEPENDENCY export data going stale in ws.depCache:
// the dependency importer decodes and caches a *types.Package by import
// path, with nothing in that path noticing that its own real content
// changed underneath it (the specific case: a file created or deleted in
// its directory — the only way needsGraphReload fires without a
// go.mod/go.sum change, since a content-only edit does not change GoFiles
// and is already covered by the per-save reindex invalidation instead, see
// Server.reindex). changedExportSet computes exactly that set of import
// paths, expanded through the reverse-dependency closure of both the
// outgoing and incoming snapshot so every importer that could have
// observed the change is covered too, and depCache.invalidate drops them
// before the reused engine's next recheck of anything importing them.
//
// The reuse condition — pointer identity with ensureDepProvider's own
// reuse decision (old.depProvider == depProvider) — piggybacks on
// ensureDepProvider's depsKey check on purpose: any change to the
// non-workspace (standard library/module-cache) package set already forces
// ensureDepProvider to rebuild depProvider, and that is exactly the signal
// that also forces engine/graphSrc/depCache to rebuild from scratch here,
// since a genuine dependency-set change (a go.mod/go.sum/go.work edit) can
// invalidate far more than changedExportSet's own GoFiles-diff heuristic
// accounts for.
func (s *Server) setWorkspace(root string, snap *graph.Snapshot) {
	s.setWorkspaceMu.Lock()
	defer s.setWorkspaceMu.Unlock()

	old := s.ws.Load()
	depProvider, depExportProvider, depExports := s.ensureDepProvider(snap)
	reuse := old != nil && old.root == root && old.depProvider == depProvider

	var (
		graphSrc     *check.GraphSource
		depCache     *depCacheHolder
		engine       *check.Engine
		rootFallback *check.Engine
	)
	if reuse {
		graphSrc = old.graphSrc
		graphSrc.Retarget(snap)
		depCache = old.depCache
		depCache.invalidate(changedExportSet(old.snap, snap))
		engine = old.engine
		rootFallback = old.rootFallback
	} else {
		graphSrc = check.NewGraphSource(snap, s.overlay)
		depCache = newDepCacheHolder(depExports, depProvider, depExportProvider, func() bool { return s.idx.Load() != nil })
		// rootExport resolves a ROOT package's already-persisted export data
		// straight from the facts index (see its own doc): built once here,
		// bound to graphSrc and to a closure reading s.idx live, so it stays
		// correct across every later setWorkspace reuse and index (re)open
		// without needing its own retargeting — see newRootExportSource's
		// doc for why.
		rootExport := newRootExportSource(graphSrc, s.overlay, func() *indexState { return s.idx.Load() }, RelativeIndexPaths(root))
		// engine's own dependency Importer is engineImporter, not
		// depCache.importer directly: it resolves a ROOT-package import
		// through the facts index's own persisted export data first
		// (decodeRoot), falling back to rootFallback's own small Get/commit
		// cache only for whatever decodeRoot could not answer — never to
		// engine's own cache (see rootFallback's own doc just below for
		// why). ei.fallback is set right after check.New(rootFallback's
		// constructor call) returns, since check.New needs ei.importer
		// before the Engine it feeds exists; check.New never calls it
		// synchronously, only later during an actual recheck, so this
		// two-phase ordering is safe — mirrors engine's own identical
		// bootstrap just below.
		ei := &engineImporter{depCache: depCache, graphSrc: graphSrc, rootExport: rootExport}
		engine = check.New(graphSrc, s.overlay, ei.importer, check.Options{OnResult: s.publishDiagnostics})
		// rootFallback is a SEPARATE, small Engine dedicated to resolving a
		// root import decodeRoot could not answer (no index yet for it, an
		// open/dirty file, or a stale blob) — deliberately never engine
		// itself. engine's own cache exists to serve the files THIS
		// session's user actually has open; sharing it with root-import
		// resolution would let an editor-driven cross-package query evict
		// those entries, and — as an earlier revision of this method did —
		// tempt sizing its cap to the workspace's own root-package count to
		// avoid thrashing a large closure, which grows a check.CheckedPackage
		// cache (full AST plus *types.Info per entry, tens of MB apiece for
		// a generated protobuf package) to workspace scale: ~400-500 entries
		// measured at 10-25GB server RSS on a large monorepo (see this
		// package's own cold-start RSS investigation notes) — the exact
		// unbounded-memory regression this split undoes.
		//
		// rootFallback is left at check.Engine's own small, editor-session
		// default (MaxLRU 6, same as engine's own unset default) rather than
		// scaled to workspace size: decodeRoot already serves the common
		// case (any root package the facts index has current data for,
		// which is most of the workspace once warm), so rootFallback only
		// ever needs to hold the small, session-scale set of packages
		// currently stale or dirty at once. This accepts some LRU thrashing
		// under a closure with many SIMULTANEOUSLY stale root packages (see
		// TestEngine_Get_RootImportCacheThrashingIsACacheSizingProblem in
		// internal/check) in exchange for memory bounded independent of
		// workspace size — the same tradeoff depcheck.DefaultCap's own doc
		// makes for interactive non-root navigation. rootFallback shares
		// ei.importer as its OWN dependency Importer too, so a package it
		// checks that itself imports another stale root package recurses
		// back into rootFallback (see check.Engine.GetPackage's own doc for
		// why that recursion is safe) rather than into engine or into
		// depCache's non-persistent depexport path.
		rootFallback = check.New(graphSrc, s.overlay, ei.importer, check.Options{})
		ei.fallback = rootFallback
		// Retire, not Stop, the outgoing engine: a debounce timer already
		// scheduled on it (e.g. by a handleDidChange that captured the old
		// workspace microseconds before this swap), or a background
		// recheck still running against the now-discarded import graph,
		// could otherwise still fire afterward and publish diagnostics via
		// Options.OnResult computed against stale state — Retire's timer
		// cancellation and OnResult suppression (see its own doc) prevent
		// exactly that. Unlike Stop, Retire does not cancel the engine's
		// own lifecycle ctx, so a request-driven Get flight already in
		// progress against the old engine (e.g. a hover the user
		// triggered microseconds before this reload) keeps running to
		// completion and its waiter gets the real result instead of
		// ctx.Err(). The outgoing rootFallback gets the identical Retire
		// treatment: it never publishes (its own Options.OnResult is nil,
		// so Retire's publish suppression is a no-op for it), but a
		// GetPackage flight already in flight against it deserves the same
		// run-to-completion guarantee engine's own in-flight Get gets.
		if old != nil {
			old.engine.Retire()
			old.rootFallback.Retire()
		}
	}

	fileToPkg := make(map[string]string)
	dirToPkg := make(map[string]string, len(snap.Packages))
	for pkgPath, pkg := range snap.Packages {
		if pkg.ForTest == "" {
			for _, f := range pkg.GoFiles {
				fileToPkg[f] = pkgPath
			}
			dirToPkg[pkg.Dir] = pkgPath
			continue
		}
		// A ForTest-tagged entry (most commonly a directory's external
		// "_test" package) never gets a dirToPkg entry: it can share pkg.Dir
		// with the ordinary package it tests under a different PkgPath — see
		// internal/check.GraphSource's identical exclusion — so indexing it
		// here too would risk misrouting a brand-new unsaved file in that
		// directory to an unimportable PkgPath. It DOES get fileToPkg
		// entries for its own already-known files when it is genuinely a
		// workspace directory's own external test package (isExternalTestOfRoot,
		// mirroring internal/index's identical predicate): those exact file
		// paths can never collide with the base package's own GoFiles, so
		// there is nothing to misroute, and without this a didOpen/didSave
		// on such a file would resolve to the base package's pkgPath
		// instead of its own — see nonWorkspacePackageForFile, which this
		// pairs with to keep such a file routed through ws.engine rather
		// than ws.depProvider.
		if isExternalTestOfRoot(snap, pkg) {
			for _, f := range pkg.GoFiles {
				fileToPkg[f] = pkgPath
			}
		}
	}
	pkgNameIndex := buildPkgNameIndex(snap)

	newWS := &workspace{
		root: root, snap: snap, graphSrc: graphSrc, engine: engine, rootFallback: rootFallback, fileToPkg: fileToPkg, dirToPkg: dirToPkg,
		depCache: depCache, depProvider: depProvider, pkgNameIndex: pkgNameIndex,
	}
	s.ws.Store(newWS)
	// Unblocks any waitWorkspace caller (checkedFile) parked from before
	// this, the workspace's first ever install — a no-op on every later
	// setWorkspace call (revalidateGraph, a watched-files-triggered
	// reload), since the channel is already closed by then. s.wsReady is
	// nil for a Server built by test code as a bare &Server{...} literal
	// rather than through New (a pattern several existing unit tests use
	// for a minimal server with only the fields their own test needs) —
	// harmless to skip there, since such a test never calls waitWorkspace
	// concurrently with this.
	if s.wsReady != nil {
		s.wsReadyOnce.Do(func() { close(s.wsReady) })
	}
	// A no-op past the workspace's first install: handleDidOpen only ever
	// queues a path via markPendingOpen while s.workspace() is nil, so
	// nothing is pending here again once it is not.
	s.drainPendingOpens(newWS)

	if idx := s.idx.Load(); idx != nil {
		s.idx.Store(&indexState{db: idx.db, cas: idx.cas, resolver: s.newResolver(idx.db, idx.cas, snap, RelativeIndexPaths(root))})
	}

	s.refreshOnWorkspaceReady()
}

// buildPkgNameIndex indexes every package in snap by its declared name, for
// unimported-package completion (see workspace.pkgNameIndex's doc). A
// package with no Name (a pre-Name-field on-disk cache — see
// graph.cacheVersion — or a package go/packages could not resolve a name
// for), named "main" (never importable), or ForTest-tagged (a synthesized
// test-only node — e.g. an external "_test" package — never a path
// anything can legitimately import) is skipped; the "main" exclusion
// mirrors gopls's own unimportedPackages, which excludes "main" packages
// from its candidate set the same way.
func buildPkgNameIndex(snap *graph.Snapshot) map[string][]string {
	idx := make(map[string][]string)
	for path, pkg := range snap.Packages {
		if pkg.Name == "" || pkg.Name == "main" || pkg.ForTest != "" {
			continue
		}
		idx[pkg.Name] = append(idx[pkg.Name], path)
	}
	for name := range idx {
		sort.Strings(idx[name])
	}
	return idx
}

// refreshOnWorkspaceReady tells a capability-declaring client that
// workspace-wide state it may have cached (inlay hints, semantic tokens) can
// now be re-requested, because setWorkspace just installed a new snapshot —
// either the very first one (handleInitialize) or a later
// reload/revalidation (revalidateGraph). Without this, a client that asked
// for inlay hints or semantic tokens before this workspace snapshot existed
// gets one empty answer (see handleInlayHint/semanticTokensForFile's
// ws == nil case) and, per the LSP spec, is not expected to re-request on
// its own — it would otherwise show nothing until an unrelated recheck
// happened to publish diagnostics and fire a refresh first.
//
// Each refresh runs detached via s.rpc.Go (not awaited here), like
// publishDiagnostics's own refresh calls: s.rpc.Request blocks until the
// client responds, and setWorkspace must not block on that, nor is this
// called while holding any lock.
func (s *Server) refreshOnWorkspaceReady() {
	for _, refresh := range s.workspaceReadyRefreshes() {
		s.rpc.Go(refresh)
	}
}

// workspaceReadyRefreshes returns the refresh calls refreshOnWorkspaceReady
// should fire, gated on client capabilities and s.clientInitialized (see its
// doc): nil whenever the client's "initialized" notification has not
// arrived yet, which is always true for the very first setWorkspace call —
// it happens synchronously inside handleInitialize, before the client can
// have sent "initialized" — so that call's own refresh is correctly
// suppressed rather than violating the LSP's server-request ordering rule.
// Split out from refreshOnWorkspaceReady so this gating decision can be
// tested without needing to observe an actual s.rpc.Go dispatch.
func (s *Server) workspaceReadyRefreshes() []func(context.Context) {
	if !s.clientInitialized.Load() {
		return nil
	}
	var refreshes []func(context.Context)
	if s.inlayHintRefreshSupport.Load() {
		refreshes = append(refreshes, s.refreshInlayHints)
	}
	if s.semanticTokensRefreshSupport.Load() {
		refreshes = append(refreshes, s.refreshSemanticTokens)
	}
	return refreshes
}

// revalidateGraph reloads the import graph from scratch and installs it as
// the current workspace, refreshing the on-disk cache. Used both for a
// stale-cache background revalidation right after initialize and for a
// workspace/didChangeWatchedFiles-triggered reload.
//
// It also revalidates the facts index (s.revalidateIndex) against the
// snapshot it just installed. This is the single choke point closing a gap
// every revalidateGraph caller shared: an earlier revalidateIndex pass —
// notably loadWorkspaceAsync's once-per-session check right after
// initialize — may have run against a since-superseded snapshot, most
// notably the shared graph cache's possibly-wrong one (every worktree of a
// repository reads and writes the same cache file; see graph.Shared and
// loadWorkspaceAsync's own doc for why it is trusted immediately rather
// than waited on), and so could never have scanned a package that
// snapshot did not even list. Revalidating here means neither this
// function's own callers nor any future one needs to separately remember
// to do so; it is cheap whenever nothing is actually stale (see
// index.RevalidateStale's own doc), so paying for it on every reload —
// not only the one that might have mattered — costs nothing worth
// special-casing around.
func (s *Server) revalidateGraph(opts graph.Options, patterns []string) {
	snap, err := graph.Load(opts, patterns...)
	if err != nil {
		s.logger.Printf("server: reload import graph: %v", err)
		return
	}
	if err := graph.SaveCache(opts.Dir, patterns, opts.BuildFlags, snap); err != nil {
		s.logger.Printf("server: save graph cache: %v", err)
	}
	s.setWorkspace(opts.Dir, snap)
	s.revalidateIndex(s.rpc.Context(), opts.Dir)
}

// handleDidChangeWatchedFiles keeps the workspace current when files change
// outside the editor — most notably a `git pull` or branch switch, which a
// client's own file watcher reports the same way it would a save. Two
// change classes are handled differently:
//
//   - go.mod, go.sum, go.work, or go.work.sum changing reloads the import
//     graph unconditionally and immediately, in the background: any of
//     these can change what packages.Load would compute for the whole
//     workspace, and such a change is comparatively rare.
//   - .go files changing (created, edited, or deleted) are first checked
//     against s.watchFP (see watchFingerprints): an event whose path's
//     on-disk (size, mtime) exactly matches what the last real event for it
//     already recorded is a no-op — some clients' watchers periodically
//     re-report unchanged files this way — and is dropped here rather than
//     handed onward. A genuine one is handed to s.watch (see watch.go),
//     which debounces and coalesces them — a `git pull` can touch thousands
//     of files in one burst — into a single revalidateWorkspace pass once
//     things go quiet.
//
// A batch containing both is handled as a go.mod-style reload only: that
// already implies everything a .go-file-driven revalidation would also
// find, so there is nothing left for s.watch to do.
func (s *Server) handleDidChangeWatchedFiles(_ context.Context, params json.RawMessage) error {
	var p protocol.DidChangeWatchedFilesParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return err
	}
	ws := s.workspace()
	if ws == nil {
		return nil
	}
	for _, ch := range p.Changes {
		if isModuleFile(ch.URI.FsPath()) {
			// s.rpc.Go, not a raw goroutine: tracks this reload the same way
			// as the server's other detached background work, so Serve's
			// shutdown-time wg.Wait (see Stop's doc) drains it instead of
			// letting quitting the editor right after a go.mod change race
			// an in-flight rebuild.
			s.rpc.Go(func(context.Context) {
				s.revalidateGraph(graph.Options{Dir: ws.root, Offline: s.opts.Offline}, []string{allPackagesPattern})
			})
			return nil
		}
	}

	sawGoFile := false
	reload := false
	var knownDirs map[string]bool
	for _, ch := range p.Changes {
		path := ch.URI.FsPath()
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if !s.watchFP.changed(path, ch.Type) {
			// Same (size, mtime) as the last event this path actually acted
			// on: an editor re-reporting a no-op (see watchFingerprints),
			// not a real change. Skip it rather than paying for a
			// workspace-wide revalidateWorkspace pass over nothing.
			continue
		}
		sawGoFile = true
		if knownDirs == nil {
			knownDirs = packageDirs(ws.snap)
		}
		if needsGraphReload(ws, knownDirs, ch) {
			reload = true
			break
		}
	}
	if sawGoFile {
		s.watch.onEvent(ws.root, reload)
	}
	return nil
}

// isModuleFile reports whether path is one of the module-structural files
// whose change can alter the import graph.
func isModuleFile(path string) bool {
	switch filepath.Base(path) {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	default:
		return false
	}
}

// needsGraphReload reports whether ch, a change to a .go file, can only be
// resolved correctly by reloading the import graph (`go list`) rather than
// the cheaper path (revalidateIndex against the graph already loaded):
//
//   - a file already known to the graph (ws.fileToPkg) being deleted
//     changes that package's GoFiles, which only a reload can discover —
//     an edit to a known file does not, since revalidateIndex's own
//     per-package content hash already covers that case;
//   - a file not known to the graph landing in a directory that is already
//     a known package's directory (knownDirs) also changes that package's
//     GoFiles, whether the event says Created or Changed (some clients
//     coalesce a create immediately followed by a write into one Changed
//     event) — same reasoning, only a reload can discover it.
//
// A file landing in a directory the graph has never seen at all is a
// brand-new package. Discovering that would need its own `go list` on
// every single such event to even find out (unlike the two cases above,
// where knownDirs/ws.fileToPkg already answer it for free), so this v0.1
// scope deliberately does not: a brand-new package is picked up on the
// next restart instead, rather than paying a `go list` per stray Created
// event (e.g. a scratch file dropped in the workspace outside any package,
// or transient files an external tool creates and removes in one burst).
func needsGraphReload(ws *workspace, knownDirs map[string]bool, ch protocol.FileEvent) bool {
	path := ch.URI.FsPath()
	if _, known := ws.fileToPkg[path]; known {
		return ch.Type == protocol.FileChangeTypeDeleted
	}
	return knownDirs[filepath.Dir(path)]
}

// packageDirs returns the set of directories snap already has a package
// for, used by needsGraphReload to recognize a new file landing inside an
// already-known package.
func packageDirs(snap *graph.Snapshot) map[string]bool {
	dirs := make(map[string]bool, len(snap.Packages))
	for _, pkg := range snap.Packages {
		dirs[pkg.Dir] = true
	}
	return dirs
}

// revalidateWorkspace re-checks root's workspace after a debounced batch of
// external .go file changes (see watch.go). If reload is true, the import
// graph is reloaded from scratch first (see revalidateGraph) — a
// new/removed file in an already-known package can only be discovered that
// way (see needsGraphReload); a brand-new package is not discovered here at
// all, deferred to the next restart (see needsGraphReload's doc) — and
// revalidateGraph itself already revalidates the facts index against the
// snapshot it installs, so there is nothing further to do here in that
// case. Otherwise, the facts index is revalidated directly against the
// already-loaded snapshot exactly like the once-at-startup check (see
// revalidateIndex): if it disagrees with what is now on disk, the indexer
// subprocess rebuilds it (or a targeted repair fixes it in place) in the
// background exactly as it does on a cold start.
func (s *Server) revalidateWorkspace(root string, reload bool) {
	if reload {
		s.revalidateGraph(graph.Options{Dir: root, Offline: s.opts.Offline}, []string{allPackagesPattern})
		return
	}
	// s.watch (see watch.go) calls this from its own debounce-timer
	// goroutine, not from a request/notification handler, so there is no
	// handler-scoped ctx to thread through here: s.rpc.Context() is the
	// session-lifetime context that binds the indexer subprocess this may
	// launch to the server's own shutdown (see revalidateIndex).
	s.revalidateIndex(s.rpc.Context(), root)
}
