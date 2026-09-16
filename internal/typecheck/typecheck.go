// Package typecheck runs go/types over a single package's already-parsed
// files, resolving its dependencies from export data rather than
// re-type-checking them. Two ExportSource values are tried in order: a
// primary one (typically self-authored blobs, e.g. from a prior
// WriteExport of a workspace package already checked earlier in the same
// run) and a fallback (typically internal/depexport.Cache, resolving a
// non-root, standard-library or module-cache dependency's export data by
// declaration-only source-checking it — never by invoking the Go
// toolchain's own compiler; see that package's own doc for why). Either may
// be nil to skip that tier.
package typecheck

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sync"

	"golang.org/x/sync/singleflight"
	"golang.org/x/tools/go/gcexportdata"
)

// ExportSource resolves export data for a package, keyed by import path. ok
// is false when the source has no data for pkgPath — the importer then
// falls back to its next configured ExportSource, if any (see NewImporter).
type ExportSource interface {
	ExportData(pkgPath string) (data []byte, ok bool, err error)
}

// Cache holds decoded *types.Package values keyed by import path, shared
// across Importer instances so package identity survives multiple
// CheckPackage calls. Callers own its lifetime: create one to share type
// identity across a batch of related checks, discard it to release memory.
//
// A Cache is tied to the single *token.FileSet its entries were decoded
// into (gcexportdata.Read registers position information into that fset as
// a side effect of decoding). Callers that keep a Cache alive across many
// CheckPackage calls — e.g. a long-lived check engine — must reuse the same
// fset for every decode against it, and must discard the Cache and its
// fset together, never independently.
type Cache struct {
	mu      sync.Mutex
	pkgs    map[string]*types.Package
	sizes   map[string]int64 // pkgPath -> its decode's size, the same value summed into bytes below
	failed  map[string]error // pkgPath -> ReadExport's error, see ReadExport's doc
	bytes   int64            // sum of sizes for entries currently in pkgs, a naive proxy for memory held
	decodes int64            // number of gcexportdata.Read calls this Cache has performed (cache misses)

	pins    map[string]int32    // pkgPath -> total pin count from every source below; see pinLocked
	pending map[string]bool     // pkgPath -> a Delete arrived while pinned; applied once the last pin releases
	deps    map[string][]string // pkgPath -> the paths ITS OWN decode pinned on its behalf (see claimLocked); unpinned when pkgPath itself is removed
	resets  int64               // count of resetUnpinnedLocked calls (decode self-heals); see Resets
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{
		pkgs: make(map[string]*types.Package), sizes: make(map[string]int64), failed: make(map[string]error),
		pins: make(map[string]int32), pending: make(map[string]bool), deps: make(map[string][]string),
	}
}

// Delete removes pkgPath's cached *types.Package and any cached ReadExport
// failure for it, if either exists, subtracting its recorded size from
// bytes so bytes reflects only entries still cached rather than growing
// monotonically forever. A later ImportFrom/ReadExport call for pkgPath
// re-decodes it from export data instead of serving a stale success or a
// stale failure. Callers use this to evict dependencies once every importer
// that needed them has finished, bounding cache growth independent of
// workspace size, and to invalidate a reindexed package's entry (see
// internal/xref.Resolver.Invalidate, ReadExport's only caller that also
// calls this).
//
// If pkgPath is currently pinned — by a live CheckScope still actively
// resolving it (see pinAllLocked), or because some OTHER entry still
// cached in pkgs was decoded with pkgPath among its own dependencies (see
// claimLocked) — the removal is deferred until every pin on it releases
// (see unpinLocked) instead of happening now: a caller sharing this Cache
// across concurrent CheckPackage calls (internal/index.Build's scheduler is
// the one that actually exercises this) can otherwise evict an entry a
// DIFFERENT, still-in-flight check has already embedded into its own
// in-progress *types.Info, OR that some other, already-finished check's own
// cached result still references — via gcexportdata's shared imports map,
// which silently pulls in an incomplete placeholder for every package an
// already-decoded blob references, not just pkgPath's own direct importers
// (see computeNonRootFanIn's doc in internal/index for why that accounting
// cannot see either edge at all) — so a later ImportFrom for pkgPath, from
// that check OR from whatever still embeds the older result, would
// otherwise decode a second, non-identical *types.Package, exactly the
// identity split #116 fixed for internal/depcheck's own separate cache.
func (c *Cache) Delete(pkgPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pins[pkgPath] > 0 {
		c.pending[pkgPath] = true
		return
	}
	c.deleteLocked(pkgPath)
}

// deleteLocked performs Delete's actual removal, including releasing every
// pin pkgPath's own decode placed on its behalf (see claimLocked) — which
// may in turn make one of THOSE paths eligible for its own pending Delete,
// applied recursively by unpinLocked. c.mu must already be held.
func (c *Cache) deleteLocked(pkgPath string) {
	delete(c.pkgs, pkgPath)
	delete(c.failed, pkgPath)
	c.bytes -= c.sizes[pkgPath]
	delete(c.sizes, pkgPath)
	delete(c.pending, pkgPath)
	deps := c.deps[pkgPath]
	delete(c.deps, pkgPath)
	for _, dep := range deps {
		c.unpinLocked(dep)
	}
}

// pinAllLocked pins pkg itself, plus every package pkg.Imports() reports
// (see CheckScope's own doc for why that set — not just pkg's own path —
// is what needs protecting against a concurrent Delete), atomically with
// whatever cache lookup or decode produced pkg — c.mu must already be held.
// Returns every path pinned, for the caller to release later (see
// CheckScope.Close). Establishing this INSIDE the same locked section that
// resolved pkg (rather than as a separate, later call, this package's own
// pre-fix design) closes the race window where a concurrent Delete could
// observe zero pins between pkg's resolution succeeding and a separate pin
// call re-acquiring the lock.
func (c *Cache) pinAllLocked(pkg *types.Package) []string {
	paths := make([]string, 0, 1+len(pkg.Imports()))
	paths = append(paths, pkg.Path())
	for _, imported := range pkg.Imports() {
		paths = append(paths, imported.Path())
	}
	for _, p := range paths {
		c.pinLocked(p)
	}
	return paths
}

// claimLocked pins pkg.Imports() on pkgPath's own behalf, for as long as
// pkgPath itself stays cached — released by deleteLocked when pkgPath is
// finally removed, not by any particular check's own lifetime. This is
// decode's own cached-lifetime counterpart to pinAllLocked's in-flight
// protection: two DIFFERENT top-level checks reaching the same shared
// dependency through two DIFFERENT already-cached packages — neither of
// which is itself still being actively resolved by a live CheckScope, so
// pinAllLocked's own in-flight pins on them have long since released — can
// still each transitively reference a common package whose OWN cache entry
// must not be evicted out from under either of them. Chaining one level of
// direct pins per cached entry (pkgPath pins its own Imports(), which
// themselves pin THEIR OWN Imports() for as long as THEY stay cached, and
// so on) is sufficient: a package survives exactly as long as anything that
// transitively depends on it does, without pkgPath needing to compute or
// store its own full transitive closure. c.mu must already be held; pkgPath
// itself is not pinned here — only its dependencies, on pkgPath's behalf.
func (c *Cache) claimLocked(pkgPath string, pkg *types.Package) {
	deps := make([]string, 0, len(pkg.Imports()))
	for _, imported := range pkg.Imports() {
		deps = append(deps, imported.Path())
	}
	c.deps[pkgPath] = deps
	for _, dep := range deps {
		c.pinLocked(dep)
	}
}

// pinLocked increments pkgPath's pin count; c.mu must already be held.
func (c *Cache) pinLocked(pkgPath string) {
	c.pins[pkgPath]++
}

// unpinLocked releases one pin acquired by pinLocked/pinAllLocked/
// claimLocked, applying a Delete that arrived while pinned (see c.pending)
// once pkgPath's last pin releases — which may itself cascade into further
// deleteLocked calls for whatever THAT removal was itself the last pin on.
// c.mu must already be held.
func (c *Cache) unpinLocked(pkgPath string) {
	c.pins[pkgPath]--
	if c.pins[pkgPath] > 0 {
		return
	}
	delete(c.pins, pkgPath)
	if c.pending[pkgPath] {
		c.deleteLocked(pkgPath)
	}
}

// unpin releases one pin on pkgPath (see unpinLocked).
func (c *Cache) unpin(pkgPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unpinLocked(pkgPath)
}

// unpinN releases n pins on pkgPath in one locked call (see unpinLocked) —
// CheckScope's own batch release for a path its own resolution pinned more
// than once (see CheckScope.touched's doc).
func (c *Cache) unpinN(pkgPath string, n int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for range n {
		c.unpinLocked(pkgPath)
	}
}

// Len returns the number of *types.Package values currently cached.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pkgs)
}

// Bytes returns the sum of decoded export-data blob sizes for entries
// currently cached (Delete subtracts an evicted entry's size): a cheap,
// approximate estimate of the memory c is holding onto, for callers that
// want to bound cache growth (e.g. discard c and start a fresh one past
// some threshold) without a precise heap accounting.
func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Decodes returns the number of times c has actually run gcexportdata.Read
// (i.e. cache misses), as opposed to being served from an already-decoded
// entry. Test-observability hook for asserting that a warm Cache avoids
// redundant decode work.
func (c *Cache) Decodes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.decodes
}

// FailedLen returns the number of pkgPaths currently holding a cached
// ReadExport failure. Test-observability hook for asserting that a
// package whose export data fails to decode is recorded (see ReadExport's
// doc for why this matters: without it, the same expensive failed decode
// repeats on every call for that pkgPath).
func (c *Cache) FailedLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.failed)
}

// Resets returns the number of times decode has self-healed a
// gcexportdata.Read failure by discarding every currently-unpinned entry
// and retrying (see (*Importer).decode's own doc). Test-observability hook
// for confirming the self-heal actually fires, in a fixture test and
// against a real corpus run (internal/index.Build surfaces this via
// Stats.DecodeSelfHeals) alike.
func (c *Cache) Resets() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resets
}

// resetUnpinnedLocked discards every entry in pkgs/failed NOT currently
// held live by a CheckScope (see pin's own doc) — decode's own condemn-
// and-heal response to a gcexportdata.Read failure. c.mu must already be
// held.
//
// Deliberately pin-aware rather than a blanket clear: an entry still
// pinned by a live CheckScope is, by construction, already embedded in
// that check's own in-progress *types.Info — discarding it here would
// reintroduce the exact identity split pin/Delete's own deferred-eviction
// already prevents (see Delete's doc), just via a different trigger. An
// entry NOT pinned is, by the same construction, not yet observed by
// anything currently mid-check, so clearing it (and letting the retry that
// follows re-decode it fresh, or reuse whatever a different, still-live
// pin already holds for the very same path — see decode's own doc) cannot
// split any check's own identity, no matter which check's decode triggered
// the reset.
func (c *Cache) resetUnpinnedLocked() {
	for path := range c.pkgs {
		if c.pins[path] == 0 {
			c.deleteLocked(path)
		}
	}
	for path := range c.failed {
		if c.pins[path] == 0 {
			c.deleteLocked(path)
		}
	}
}

// Importer implements types.ImporterFrom over two ExportSource tiers,
// decoding through gcexportdata and caching results in cache. Not safe for
// concurrent use across multiple Importer values sharing the same Cache
// without external synchronization beyond what Cache itself provides. A
// single Importer value, however, is designed for concurrent ImportFrom
// calls (see its doc): only the map-mutating decode step holds Cache's
// lock, and sf collapses concurrent callers requesting the same path onto
// one decode.
type Importer struct {
	fset     *token.FileSet
	src      ExportSource
	fallback ExportSource
	cache    *Cache
	sf       singleflight.Group
}

// NewImporter returns an Importer that resolves imports via src first, then
// fallback, decoding into fset and caching results in cache. Either src or
// fallback may be nil to skip that tier.
func NewImporter(fset *token.FileSet, src, fallback ExportSource, cache *Cache) *Importer {
	return &Importer{fset: fset, src: src, fallback: fallback, cache: cache}
}

// Import implements types.Importer.
func (imp *Importer) Import(path string) (*types.Package, error) {
	return imp.ImportFrom(path, "", 0)
}

// NewCheck returns a CheckScope for one CheckPackage call sharing imp's
// Cache — see CheckScope's own doc for why a caller whose Cache is shared
// across concurrent CheckPackage calls (internal/index.Build's scheduler)
// needs one.
func (imp *Importer) NewCheck() *CheckScope {
	return &CheckScope{imp: imp, touched: make(map[string]int32)}
}

// CheckScope pins, for one CheckPackage call's entire duration, every
// package that call's own import resolution touches — directly, or
// transitively as referenced inside another package's already-decoded
// export data — so a concurrently-finishing, unrelated CheckPackage call
// sharing the same Cache can never make Cache.Delete remove an entry this
// scope has already embedded into its own in-progress *types.Info. This is
// the IN-FLIGHT half of Cache's own protection: pins here live only as long
// as this one CheckScope does. The complementary, CACHED-LIFETIME half
// (claimLocked, driven by decode) protects a dependency for as long as some
// OTHER, already-finished check's own cached result still embeds it, well
// past any CheckScope's own lifetime — both contribute to the identical
// Cache.pins refcount, so an entry survives as long as either source still
// needs it.
//
// This exists because gcexportdata's shared imports map (Cache.pkgs) does
// far more than resolve each package's own direct imports: decoding one
// package's export data walks that package's ENTIRE reference manifest —
// every OTHER package any of its exported declarations names, at any type
// nesting depth — creating an incomplete placeholder *types.Package for any
// of them not already present (golang.org/x/tools/internal/gcimporter's
// GetPackagesFromMap), inserted into the exact same map a later, unrelated
// decode call shares. types.Package.Imports() after a successful
// ImportFrom/decode reports exactly that reference set (see
// golang.org/x/tools/internal/gcimporter/ureader.go's readUnifiedPackage:
// "Imports() of pkg are all of the transitive packages that were loaded"),
// so pinAllLocked captures it precisely, one level, no further recursion
// needed — a placeholder homes only the specific named types referenced
// through it and has no further imports of its own to chase.
//
// internal/index's own scheduler evicts a dependency from the shared Cache
// once every DIRECT root-package importer it knows about has finished
// (computeNonRootFanIn) — accounting with no visibility at all into a
// dependency only ever reached this indirect way (never a direct import of
// any root package by itself). Without CheckScope pinning that entry for
// the whole time this call's own resolution embeds it, that eviction could
// remove it between two ImportFrom calls within THIS SAME check, and a
// later one for the identical import path would decode a second,
// non-identical *types.Package for it — go/types compares named types (and
// satisfies generic instantiations) by object identity, not structural
// shape, so the two non-identical instances feeding into one
// Checker.Check call make an otherwise-valid generic instantiation fail an
// interface-satisfaction check `go build` accepts. This is the exact same
// mechanism internal/depcheck's own closureScope fixed (#116) for its
// separate, per-Provider cache; CheckScope is that fix's counterpart for
// the shared typecheck.Cache internal/index.Build's scheduler drives.
//
// Create one via Importer.NewCheck for each top-level CheckPackage call
// sharing that Importer's Cache, and call Close once that call returns —
// never reuse a CheckScope across two CheckPackage calls, and never skip
// Close, or its pins leak for the Cache's remaining lifetime. A CheckScope
// is used from a single goroutine only — the same goroutine that drives the
// CheckPackage call it was created for, since types.Config.Check invokes
// ImportFrom synchronously — so touched needs no lock of its own; imp's own
// Cache still serializes every pin/unpin and cache access it triggers.
type CheckScope struct {
	imp *Importer
	// touched counts, per path, how many pins THIS scope has accumulated
	// for it: importFromPinned re-pins path and its own Imports() on every
	// FIRST resolution of a given top-level path (see ImportFrom's own
	// doc), and the same path can be pinned again as a member of a
	// DIFFERENT top-level import's own Imports() set — a plain set would
	// under-count Close's own release against pinAllLocked's own,
	// possibly-repeated increments.
	touched map[string]int32
}

// Close releases every pin s acquired over its lifetime (see CheckScope's
// doc). Call exactly once, after the CheckPackage call s was created for
// has returned.
func (s *CheckScope) Close() {
	for path, n := range s.touched {
		s.imp.cache.unpinN(path, n)
	}
}

// Import implements types.Importer.
func (s *CheckScope) Import(path string) (*types.Package, error) {
	return s.ImportFrom(path, "", 0)
}

// ImportFrom implements types.ImporterFrom, delegating to s's own Importer.
//
// path already in s.touched means THIS scope has already pinned it (as a
// prior top-level target, or as a member of some other top-level import's
// own Imports() set — see importFromPinned's doc): the entry is guaranteed
// to still be a cache hit (nothing can have evicted it while s's own pin is
// held), so this takes the plain, non-pinning path — no new atomicity
// concern, since nothing new needs protecting.
//
// Otherwise this is s's own first touch of path: importFromPinned resolves
// AND pins path (plus its own Imports()) atomically, closing the race
// window a separate, later pin call would leave open (this package's own
// pre-fix design — see Cache.pinAllLocked's doc) — every path it pinned is
// recorded into s.touched for Close to release later.
func (s *CheckScope) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if s.touched[path] > 0 {
		return s.imp.ImportFrom(path, dir, mode)
	}
	pkg, pinned, err := s.imp.importFromPinned(path)
	if err != nil {
		return nil, err
	}
	for _, p := range pinned {
		s.touched[p]++
	}
	return pkg, nil
}

// unsafePkgPath is the predeclared "unsafe" pseudo-package. go/types has no
// built-in handling for it (see go/types.Checker.importPackage: it calls
// Config.Importer.ImportFrom("unsafe", ...) exactly like any other import
// path) — every Importer implementation is expected to special-case it
// itself, which is why go/internal/gcimporter.Import,
// x/tools/internal/gcimporter.Import, and gcexportdata.NewImporter's own
// ImportFrom all check path == "unsafe" and return types.Unsafe directly,
// before ever touching their own decode machinery. types.Unsafe is not
// decodable/encodable export data at all — gcexportdata.Write (called via
// WriteExport, e.g. by internal/depexport when a workspace package that
// imports "unsafe" reached this Importer's fallback tier) panics
// unconditionally trying to serialize it (iexporter.pushDecl: "cannot
// export package unsafe").
const unsafePkgPath = "unsafe"

// ImportFrom implements types.ImporterFrom. dir and mode are accepted for
// interface compliance but unused: export data resolution here is keyed
// purely by import path.
//
// Concurrent ImportFrom calls (from concurrent CheckPackage runs sharing
// this Importer) only serialize on Cache's lock for the brief map lookup
// and, per distinct path, the gcexportdata.Read call that mutates the
// shared imports map. Resolving the export data itself — an ExportSource
// lookup against src, then fallback — runs outside that lock, and
// singleflight collapses concurrent callers for the same uncached path onto
// a single resolve instead of each repeating the work.
func (imp *Importer) ImportFrom(path, _ string, _ types.ImportMode) (*types.Package, error) {
	if path == unsafePkgPath {
		return types.Unsafe, nil
	}
	if pkg, ok := imp.cacheGet(path); ok {
		return pkg, nil
	}

	v, err, _ := imp.sf.Do(path, func() (any, error) {
		if pkg, ok := imp.cacheGet(path); ok {
			return pkg, nil
		}
		return imp.resolve(path)
	})
	if err != nil {
		return nil, err
	}
	pkg, ok := v.(*types.Package)
	if !ok {
		return nil, fmt.Errorf("typecheck: singleflight for %s returned %T, want *types.Package", path, v)
	}
	return pkg, nil
}

// importFromPinned is CheckScope's own entry point into imp: resolves path
// exactly like ImportFrom, additionally pinning path and every package
// pkg.Imports() reports (see Cache.pinAllLocked's own doc), atomically with
// whichever cache lookup or decode actually resolves them, closing the race
// window a separate, later pin call would leave open (this package's own
// pre-fix design): between a successful resolution releasing cache.mu and a
// separate pin call re-acquiring it, a concurrent Delete could observe zero
// pins and remove the very entry just resolved.
//
// Both the cache-hit path and the leader side of a singleflight-collapsed
// decode pin atomically, inside the very lock that produced or found pkg.
// A singleflight FOLLOWER's own pin registration still necessarily happens
// after its leader's own call released that lock (singleflight only wakes
// every waiter once the leader's own function has returned) — narrow in
// practice: path's own fan-in cannot reach zero until every DIRECT importer
// of it, which every singleflight participant here necessarily is, has
// itself finished its own check, well past the point any of them could
// still be waiting on this call. decode's own claimLocked call additionally
// covers this window independent of any CheckScope at all, by pinning
// path's dependencies on path's own cached-lifetime behalf the moment it is
// decoded — see Cache.claimLocked's doc.
func (imp *Importer) importFromPinned(path string) (pkg *types.Package, pinned []string, err error) {
	if path == unsafePkgPath {
		return types.Unsafe, nil, nil
	}

	imp.cache.mu.Lock()
	if p, ok := imp.cache.pkgs[path]; ok && p.Complete() {
		pinned = imp.cache.pinAllLocked(p)
		imp.cache.mu.Unlock()
		return p, pinned, nil
	}
	imp.cache.mu.Unlock()

	v, err, _ := imp.sf.Do(path, func() (any, error) {
		imp.cache.mu.Lock()
		if p, ok := imp.cache.pkgs[path]; ok && p.Complete() {
			imp.cache.mu.Unlock()
			return p, nil
		}
		imp.cache.mu.Unlock()
		return imp.resolve(path)
	})
	if err != nil {
		return nil, nil, err
	}
	p, ok := v.(*types.Package)
	if !ok {
		return nil, nil, fmt.Errorf("typecheck: singleflight for %s returned %T, want *types.Package", path, v)
	}
	imp.cache.mu.Lock()
	pinned = imp.cache.pinAllLocked(p)
	imp.cache.mu.Unlock()
	return p, pinned, nil
}

// cacheGet returns path's cached, fully-decoded *types.Package, if any.
func (imp *Importer) cacheGet(path string) (*types.Package, bool) {
	imp.cache.mu.Lock()
	defer imp.cache.mu.Unlock()
	pkg, ok := imp.cache.pkgs[path]
	return pkg, ok && pkg.Complete()
}

// resolve locates path's export data — via imp.src first, then
// imp.fallback — and decodes it. Both are self-contained blobs with no
// archive header (see WriteExport's doc), so either is fed straight to
// gcexportdata.Read via decode, unlike a GOCACHE-generated `go list
// -export` file (which this package no longer resolves at all — see the
// package doc). Locating the data (an ExportSource lookup) does not touch
// imp.cache and so needs no lock; only decode does.
func (imp *Importer) resolve(path string) (*types.Package, error) {
	if imp.src != nil {
		data, ok, err := imp.src.ExportData(path)
		if err != nil {
			return nil, fmt.Errorf("typecheck: read export data for %s: %w", path, err)
		}
		if ok {
			return imp.decode(data, path, int64(len(data)))
		}
	}
	if imp.fallback != nil {
		data, ok, err := imp.fallback.ExportData(path)
		if err != nil {
			return nil, fmt.Errorf("typecheck: read export data for %s: %w", path, err)
		}
		if ok {
			return imp.decode(data, path, int64(len(data)))
		}
	}
	return nil, fmt.Errorf("typecheck: no export data for %s", path)
}

// decode runs gcexportdata.Read under Cache's lock. The call both reads and
// mutates imp.cache.pkgs (the shared imports map, required so a decoded
// package's referenced types share identity with the rest of the build),
// so it cannot safely run concurrently with another decode against the
// same Cache. size is the raw export-data blob size (best effort; 0 if
// unknown), recorded in the cache's byte estimate for callers that bound
// cache growth by it.
//
// A failure is retried exactly once, into the same cache after
// resetUnpinnedLocked discards every currently-unpinned entry: data itself
// round-trips cleanly in isolation (internal/depexport.checkAndPersist's
// own self-check already confirmed that before ever handing it to an
// Importer) — a failure here means imp.cache.pkgs, shared across this
// entire Importer's lifetime, currently holds an entry data's own blob
// references that violates gcexportdata.Read's own contract ("imports[path]
// does not exist, or exists but is incomplete" — see ReadExport's doc): most
// concretely, an orphaned incomplete placeholder a since-finished check's
// own CheckScope pinned only for that check's duration (see CheckScope's
// doc), never itself completed because nothing ever directly imported it,
// left behind unpinned once that check closed. resetUnpinnedLocked's own
// doc is the safety argument for why retrying into the SAME cache, rather
// than a fresh one, cannot split identity for whichever check is currently
// calling decode: everything that check (or any other still-live one) has
// already resolved stays pinned and untouched by the reset, so the retry
// can only ever create fresh entries for exactly the stale, unpinned state
// that caused the original failure, or reuse what a live pin already
// holds — never a second, non-identical instance of something already
// embedded in an in-progress *types.Info. A second failure is surfaced
// exactly as before: data was not the poison, but whatever is still wrong
// after one clean generation is no longer a staleness this call can fix by
// retrying again.
func (imp *Importer) decode(data []byte, path string, size int64) (*types.Package, error) {
	imp.cache.mu.Lock()
	defer imp.cache.mu.Unlock()
	pkg, err := gcexportdata.Read(bytes.NewReader(data), imp.fset, imp.cache.pkgs, path)
	if err != nil {
		imp.cache.resetUnpinnedLocked()
		imp.cache.resets++
		pkg, err = gcexportdata.Read(bytes.NewReader(data), imp.fset, imp.cache.pkgs, path)
		if err != nil {
			return nil, fmt.Errorf("typecheck: decode export data for %s: %w", path, err)
		}
	}
	imp.cache.bytes += size
	imp.cache.sizes[path] = size
	imp.cache.decodes++
	imp.cache.claimLocked(path, pkg)
	return pkg, nil
}

// CheckPackage type-checks files as pkgPath using imp to resolve
// dependencies, collecting every type error instead of stopping at the
// first one. info is populated with Defs, Uses, Selections, Types, Scopes,
// Instances, and Implicits.
func CheckPackage(fset *token.FileSet, files []*ast.File, pkgPath string, imp types.ImporterFrom) (*types.Package, *types.Info, []types.Error) {
	var errs []types.Error
	conf := types.Config{
		Importer: imp,
		Error: func(err error) {
			var terr types.Error
			if errors.As(err, &terr) {
				errs = append(errs, terr)
			}
		},
	}
	info := &types.Info{
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Scopes:     make(map[ast.Node]*types.Scope),
		Instances:  make(map[*ast.Ident]types.Instance),
		Implicits:  make(map[ast.Node]types.Object),
	}
	pkg, _ := conf.Check(pkgPath, fset, files, info)
	return pkg, info, errs
}
