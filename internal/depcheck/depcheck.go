// Package depcheck provides on-demand, source-type-checked representations
// of non-workspace Go packages: standard library, module dependencies, and
// test-only imports (e.g. "testing") known to the workspace's import graph
// (internal/graph). It exists to replace internal/typecheck's export-data
// dependency resolution for NAVIGATION consumers, which is limited to
// line-accurate positions and never sees unexported types (export data
// only ever describes a package's exported API). gopls resolves these same
// dependencies the same way — by type-checking their real source, not the
// compiler's export data (see research-gopls-dependency-nav.md's Q2) — and
// this package scales that approach down to golance's on-demand, low-memory
// identity: no typerefs pruning, no shallow export-data codec, a small
// in-memory LRU instead of a durable two-tier disk+memory cache (see
// Provider's doc for the caching tradeoff this implies).
//
// internal/check's compilation of WORKSPACE packages still resolves ITS
// dependencies via gcexportdata (internal/typecheck) — that stays; export
// data remains the right tool for compilation inputs. This package is used
// only where a caller needs an exact declaration position or unexported
// visibility INTO a dependency itself (see internal/langfeat.DependencyDefinition,
// the first consumer wired to it).
package depcheck

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"sync"

	"golang.org/x/sync/singleflight"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/objectpath"

	"github.com/sivchari/golance/internal/graph"
)

// MetadataSource resolves a package's on-disk metadata: its directory, its
// non-test Go files, and its direct import paths. Satisfied by
// GraphMetadataSource (a thin adapter over *graph.Snapshot); expressed as
// its own narrow interface, rather than depending on *graph.Snapshot
// directly throughout this file, so Provider's own tests can exercise it
// against a synthetic graph with no go/packages.Load involved — mirrors
// internal/check.SnapshotSource's identical reason for existing.
type MetadataSource interface {
	// Package returns pkgPath's directory, its GoFiles (non-test Go source
	// files — see graph.Package.GoFiles's doc: an in-package _test.go file
	// is never included, matching gopls's own IgnoreFuncBodies import-only
	// checks, which likewise never need a package's own test files), and
	// its direct import paths. ok is false if pkgPath is not known.
	Package(pkgPath string) (dir string, goFiles []string, imports []string, ok bool)
}

// GraphMetadataSource adapts a *graph.Snapshot into a MetadataSource. Phase
// 1's graph already loads the full transitive closure with test variants
// (internal/graph's package doc), so every package this returns ok=false
// for is genuinely outside the workspace's import graph, not merely
// untried.
type GraphMetadataSource struct{ snap *graph.Snapshot }

// NewGraphMetadataSource returns a MetadataSource backed by snap.
func NewGraphMetadataSource(snap *graph.Snapshot) GraphMetadataSource {
	return GraphMetadataSource{snap: snap}
}

// Package implements MetadataSource.
//
// A miss on pkgPath falls back to "vendor/" + pkgPath: cmd/go vendors
// several golang.org/x/{net,crypto,text,...} packages into GOROOT/src/vendor
// for the standard library's own internal use (net/http, crypto/tls, and
// friends import them as, e.g., "golang.org/x/net/http/httpguts" in their
// own literal source text — go/packages.Load's own reported import graph
// reflects cmd/go's resolution of that reference, which is the
// "vendor/"-prefixed path, not the literal one written in the importing
// file). Without this fallback, EVERY standard-library package reachable
// through net/http, crypto/tls, or crypto/ecdsa (a large fraction of any
// real dependency closure) reports "not known to the import graph" for a
// package that IS actually present, degrading to Incomplete for a reason
// with nothing to do with any real type-identity problem. A miss on both
// the bare and vendor-prefixed path is a genuine "not known to the import
// graph" — unchanged.
func (g GraphMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	pkg, ok := g.snap.Package(pkgPath)
	if !ok {
		pkg, ok = g.snap.Package("vendor/" + pkgPath)
	}
	if !ok {
		return "", nil, nil, false
	}
	return pkg.Dir, pkg.GoFiles, pkg.Imports, true
}

// CheckedPackage is a source-type-checked non-workspace package: its
// parsed files, checked *types.Package and *types.Info, sharing the
// Provider that produced it's single persistent *token.FileSet (see
// Provider's doc for why one shared, ever-growing fset is an accepted
// tradeoff here). Doc comments come from the AST (parser.ParseComments),
// present regardless of whether bodies are checked. A CheckedPackage
// returned by Package has bodies never type-checked
// (types.Config.IgnoreFuncBodies is always true there — declarations only,
// the common case for resolving a jump target's signature/doc); one
// returned by PackageWithBodies has full statement-level Defs/Uses/Selections
// too (see its doc). Immutable once returned, so sharing one instance across
// concurrent callers (via the LRU and singleflight) needs no further
// synchronization.
type CheckedPackage struct {
	pkgPath    string
	dir        string
	files      []*ast.File
	pkg        *types.Package
	info       *types.Info
	incomplete bool   // see Incomplete's doc
	firstError string // see FirstError's doc
}

// PkgPath returns the package's import path.
func (cp *CheckedPackage) PkgPath() string { return cp.pkgPath }

// Dir returns the package's directory.
func (cp *CheckedPackage) Dir() string { return cp.dir }

// Files returns the package's parsed files, positions resolved against the
// owning Provider's FileSet.
func (cp *CheckedPackage) Files() []*ast.File { return cp.files }

// Types returns the checked *types.Package.
func (cp *CheckedPackage) Types() *types.Package { return cp.pkg }

// Info returns the *types.Info populated by the check (Defs, Uses,
// Selections, Types, Scopes, Instances, Implicits). Statement-level detail
// inside function bodies is present only for a CheckedPackage returned by
// PackageWithBodies; one returned by Package has every declaration fully
// resolved but no body-level detail (IgnoreFuncBodies).
func (cp *CheckedPackage) Info() *types.Info { return cp.info }

// Incomplete reports whether checking cp reported at least one error —
// either directly (types.Config.Error fired while checking cp's own files,
// e.g. one of its own transitive imports could not be resolved) or
// transitively (an import this check resolved was itself Incomplete — see
// ctxImporter.ImportFrom, needed because referencing an already-degraded
// import's Invalid-typed symbols does not reliably make go/types call Error
// again on its own). check's own Error callback is deliberately best-effort
// (a dependency's source is assumed to compile, so a real error there must
// still degrade to a usable, if imperfect, CheckedPackage rather than
// failing outright — see check's doc) — Incomplete exists so a caller that
// must not trust or persist a degraded result (internal/depexport's
// machine-global CAS) can tell the difference, without that best-effort
// fallback itself changing for interactive navigation callers.
func (cp *CheckedPackage) Incomplete() bool { return cp.incomplete }

// FirstError returns the message of the first error check's own Error
// callback recorded for cp's own files — "" when Incomplete is false, or
// when Incomplete is true only because an import cp resolved was itself
// already Incomplete (see Incomplete's own doc on that transitive case: no
// types.Error ever fires directly against cp's own files then, so there is
// nothing to report here beyond what that import's own CheckedPackage
// exposes). A diagnostic sample only, mirroring internal/index's identical
// checkResult.FirstError — not persisted, not part of CheckedPackage's own
// cache identity.
func (cp *CheckedPackage) FirstError() string { return cp.firstError }

// DefaultCap is the LRU's default entry capacity (Options.Cap's zero
// value): small and deliberately so — dependency navigation is bursty and
// locality-heavy (a user browses one dependency's declarations at a time,
// per the task brief this package was built against), unlike a compiler or
// gopls's own batch check, which must hold an entire build's worth of
// dependencies live at once. 64 comfortably covers "every package reachable
// by a few hops of jumping around one dependency" while bounding worst-case
// resident *types.Package/*types.Info memory to a small, constant multiple
// of one dependency's size, independent of workspace or GOPATH size.
//
// A caller using a Provider for BATCH export-data production instead — a
// full run resolving every non-root package a workspace's root packages
// import, transitively (see internal/depexport, and RecommendedCap below)
// — must NOT use this default: check's own recursive import resolution
// (ctxImporter.ImportFrom) walks the FULL transitive closure through this
// same Provider, and a dependency closure larger than DefaultCap thrashes
// the LRU badly — a widely-shared package (fmt, context, sync, ...) gets
// evicted and re-checked from scratch every time a new importer reaches it
// again, turning what should be "check every distinct package once" into
// something close to "recheck a package once per importer," a real,
// measured regression (single-digit seconds and hundreds of MB becoming
// tens of seconds and multiple GB against a few hundred real
// dependencies — see RecommendedCap's own doc).
const DefaultCap = 64

// batchCapPerWorker is RecommendedCap's per-Parallelism budget: how many
// non-root CheckedPackage entries (full AST plus *types.Package/*types.Info
// — see CheckedPackage's own doc, and note the parser always parses full
// function bodies regardless of IgnoreFuncBodies) one concurrent batch
// worker's own working set is allowed to keep the LRU holding at once.
// Chosen empirically against a real, protobuf-heavy corpus (see
// RecommendedCap's doc): at Parallelism=7 (this machine's default —
// max(1, runtime.NumCPU()/2)), a cap below ~400 measurably thrashed worse on
// BOTH wall time and peak RSS than a larger one (an evicted, still-in-demand
// package being re-checked from scratch costs more allocation churn than the
// larger cap it displaces saves), while a cap of 448 (7 * 64) tracked the
// best of the caps tried on both axes. 64 keeps that ratio while still
// bounding worst-case memory to a small, constant multiple of Parallelism
// instead of to total workspace size.
const batchCapPerWorker = 64

// RecommendedCap returns the LRU capacity a Provider dedicated to BATCH
// export-data production (internal/depexport's use — not interactive
// navigation, which should keep DefaultCap) should be constructed with.
// parallelism is the caller's own concurrent-worker budget (Options.
// Parallelism); nonRootCount is the number of non-root (stdlib/module-cache)
// packages the run may need to resolve, transitively.
//
// This used to size the cap to hold nonRootCount packages live for the
// run's ENTIRE duration — correct only for a small workspace, and the
// direct cause of an indexer run against a large, protobuf-heavy monorepo
// (~2,500 root packages) driving peak RSS well past a 12 GB safety cap: the
// cap scaled with total workspace size instead of with concurrent work in
// flight, so the LRU never actually evicted anything for the length of the
// run (see internal/index.scheduler's reference-counted eviction, which
// this Provider's own separate, unrelated cache sat outside of entirely).
// A single dependency closure larger than a small cap DOES thrash it badly
// — check's own recursive import resolution (ctxImporter.ImportFrom) walks
// the full transitive closure through this same Provider, bypassing
// internal/depexport's persistent, CAS-backed cache entirely (that cache
// only ever intercepts a TOP-LEVEL request — a root package's own direct
// import — never a recursive one; see depexport.Cache.ExportData's doc) —
// a real, measured regression against a synthetic ~370-package dependency
// closure went from ~7s/~300MB (a correctly-sized cap) to ~72s/~7GB
// (DefaultCap) purely from LRU eviction forcing the same widely-shared
// packages (fmt, context, sync, ...) to be rechecked from scratch over and
// over. Scaling the cap with parallelism instead of with nonRootCount
// keeps that headroom (concurrent workers each get their own working-set
// budget — see batchCapPerWorker) while bounding worst-case resident
// memory to a size independent of workspace size, matching this package's
// own DefaultCap's identical bounding argument for the navigation case.
// nonRootCount is still respected as an upper bound: a workspace whose
// entire non-root closure is smaller than the parallelism-scaled budget
// gains nothing from a larger cap.
func RecommendedCap(nonRootCount, parallelism int) int {
	ceiling := max(DefaultCap, parallelism*batchCapPerWorker)
	if nonRootCount < ceiling {
		return max(nonRootCount, DefaultCap)
	}
	return ceiling
}

// FullBodyDefaultCap is the full-body LRU's default entry capacity
// (Options.FullBodyCap's zero value; see PackageWithBodies). Kept much
// smaller than DefaultCap: a full-body CheckedPackage carries
// statement-level Defs/Uses/Selections for every function in the package,
// materially larger per entry than a declarations-only one, and a user has
// at most a handful of dependency files open (one dependency, browsed
// locally) at any given time — unlike DefaultCap's budget, which also has
// to absorb every transitive import touched while resolving those open
// files' own declarations.
const FullBodyDefaultCap = 8

// Options configures a Provider.
type Options struct {
	// Cap is the declarations-only LRU's entry capacity. Defaults to
	// DefaultCap when <= 0.
	Cap int
	// FullBodyCap is the full-body LRU's entry capacity (see
	// PackageWithBodies). Defaults to FullBodyDefaultCap when <= 0.
	FullBodyCap int
}

// Provider resolves non-workspace packages on demand, type-checking each
// one's real source files — never compiler or gcexportdata export data —
// so that unexported types are visible and declaration positions are
// byte-exact (a go/token.Pos, not export data's line-only encoding). Safe
// for concurrent use.
//
// Fset strategy: every package Provider ever checks is parsed into ONE
// shared, persistent *token.FileSet that only grows for Provider's entire
// lifetime — deliberately unlike a per-package fset with translated
// positions, and unlike gopls's own per-BATCH fset (research-gopls-dependency-nav.md's
// summary of Q2/Q4): a per-provider fset lets a *CheckedPackage's declared
// objects and their dependencies' objects share one coordinate space, with
// no re-basing step, for as long as the caller holds any *CheckedPackage
// this Provider produced — including one already evicted from the LRU (see
// below). This is safe to keep growing forever because a token.FileSet
// entry (added by AddFile, done once per parsed file — see check) retains
// only that file's name, base offset, size, and line-start-offset table:
// O(lines), a few bytes per line, NOT the file's source text or its parsed
// AST — those are reclaimed by the garbage collector once every
// *CheckedPackage referencing them (including any the LRU has evicted) is
// itself unreferenced. A long session touching, say, 10,000 distinct
// dependency files (a large multiple of what any real navigation session
// reaches) costs on the order of a few MB in this table — a bounded,
// slowly-growing cost the LRU's cap does not need to (and structurally
// cannot, while positions must stay valid) reclaim.
type Provider struct {
	meta         MetadataSource
	capacity     int
	fullCapacity int

	fset *token.FileSet

	sf     singleflight.Group
	fullSF singleflight.Group

	mu          sync.Mutex
	lru         *lruCache
	fullLRU     *lruCache       // full-body-checked packages (see PackageWithBodies), separate from lru so a small handful of open dependency files never evicts the much larger decl-only working set
	checked     int64           // count of Package calls that actually ran CheckPackage (cache+singleflight misses); test/observability hook.
	fullChecked int64           // count of PackageWithBodies calls that actually ran a fresh full-body check; test/observability hook.
	exports     *exportResolver // TRANSITIVE import resolution's decode fast path; nil until SetExportSource is called (see its doc)
	decoded     int64           // count of ImportFrom calls served via exports instead of a full recursive check; see Decoded

	// waiters/fullWaiters count, per pkgPath, how many callers are currently
	// between enterWait and their own exitWait/exitWaitAndPin call for a
	// singleflight-guarded miss (see enterWait's own doc) — the birth-pin
	// mechanism that closes the gap a plain "check the LRU, then pin it in a
	// later, separate locked section" would otherwise leave open between
	// put/putFull inserting a fresh entry and every caller collapsed onto
	// that same check (leader and singleflight-followers alike) getting its
	// own durable pin recorded.
	waiters     map[string]int32
	fullWaiters map[string]int32
}

// NewProvider returns a Provider resolving package metadata via meta,
// bounding its declarations-only in-memory LRU at opts.Cap entries
// (DefaultCap if <= 0) and its full-body LRU at opts.FullBodyCap entries
// (FullBodyDefaultCap if <= 0).
func NewProvider(meta MetadataSource, opts Options) *Provider {
	capacity := opts.Cap
	if capacity <= 0 {
		capacity = DefaultCap
	}
	fullCapacity := opts.FullBodyCap
	if fullCapacity <= 0 {
		fullCapacity = FullBodyDefaultCap
	}
	return &Provider{
		meta: meta, capacity: capacity, fullCapacity: fullCapacity,
		fset: token.NewFileSet(), lru: newLRUCache(capacity), fullLRU: newLRUCache(fullCapacity),
		waiters: make(map[string]int32), fullWaiters: make(map[string]int32),
	}
}

// FileSet returns the *token.FileSet every *CheckedPackage this Provider
// has ever returned shares (see Provider's doc). Positions read from any of
// them remain valid against this same *token.FileSet for the Provider's
// entire lifetime.
func (p *Provider) FileSet() *token.FileSet { return p.fset }

// Len returns the number of packages currently held in the declarations-only
// LRU.
func (p *Provider) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lru.len()
}

// Checked returns the number of times Package has actually run a fresh
// type-check (as opposed to being served from the LRU or collapsed onto a
// concurrent in-flight check by singleflight). Test-observability hook,
// mirroring internal/typecheck.Cache.Decodes.
func (p *Provider) Checked() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checked
}

// LenWithBodies returns the number of packages currently held in the
// full-body LRU (see PackageWithBodies).
func (p *Provider) LenWithBodies() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fullLRU.len()
}

// CheckedWithBodies returns the number of times PackageWithBodies has
// actually run a fresh full-body check. Test-observability hook, mirroring
// Checked.
func (p *Provider) CheckedWithBodies() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fullChecked
}

// Package returns pkgPath's source-type-checked CheckedPackage (declarations
// only — see CheckedPackage's doc), checking it on demand if not already
// cached. Concurrent calls for the same pkgPath collapse onto a single check
// (singleflight); calls for different pkgPaths run independently and in
// parallel. If pkgPath is already held in the full-body LRU (see
// PackageWithBodies), that CheckedPackage is returned instead of running a
// second, redundant declarations-only check — sharing identity between the
// two call sites is exactly the point (see PackageWithBodies's doc).
//
// Every package pkgPath's own transitive closure resolves through during
// this one call is pinned — IN-FLIGHT, in a fresh closureScope for the
// call's entire duration, released once this call returns — AND, for
// whichever of them get freshly cached by this call (or already were,
// CACHED-LIFETIME, for as long as they stay cached (see closureScope's own
// doc for how these two pin sources compose): p's shared LRU can safely
// evict any of them to make room for a different, concurrently in-flight
// closure only once nothing — no longer-in-flight closure, no other
// still-cached entry — depends on it anymore, so this call will never
// itself see two different generations of the same import path as a
// result, and neither will any OTHER closure reusing what this one cached.
func (p *Provider) Package(ctx context.Context, pkgPath string) (*CheckedPackage, error) {
	scope := newClosureScope()
	defer scope.close(p)
	return p.packageScoped(ctx, pkgPath, scope)
}

// packageScoped is Package's own implementation, additionally threading
// scope through every nested resolution this call triggers (see
// closureScope's own doc and ctxImporter.scope). Callers other than
// Package itself (i.e. recursive calls from within a check already holding
// a scope) do not need — and must not add — their own scope.close, since
// the top-level call that created scope owns releasing it.
func (p *Provider) packageScoped(ctx context.Context, pkgPath string, scope *closureScope) (*CheckedPackage, error) {
	if cp, ok := scope.get(pkgPath); ok {
		return cp, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pkgPath == unsafePkgPath {
		return unsafePackage(), nil
	}
	if cp, ok := p.tryPinFull(pkgPath, scope); ok {
		return cp, nil
	}
	if cp, ok := p.tryPin(pkgPath, scope); ok {
		return cp, nil
	}

	p.enterWait(pkgPath)
	v, err, _ := p.sf.Do(pkgPath, func() (any, error) {
		if cp, ok := p.getFull(pkgPath); ok {
			return cp, nil
		}
		if cp, ok := p.get(pkgPath); ok {
			return cp, nil
		}
		cp, err := p.check(ctx, pkgPath, false, scope)
		if err != nil {
			return nil, err
		}
		p.put(pkgPath, cp)
		return cp, nil
	})
	if err != nil {
		p.exitWait(pkgPath)
		return nil, err
	}
	cp, ok := v.(*CheckedPackage)
	if !ok {
		p.exitWait(pkgPath)
		return nil, fmt.Errorf("depcheck: singleflight for %s returned %T, want *CheckedPackage", pkgPath, v)
	}
	p.exitWaitAndPin(pkgPath, cp, scope)
	return cp, nil
}

// PackageWithBodies returns pkgPath's source-type-checked CheckedPackage
// WITH full function-body type information (statement-level Defs/Uses/
// Selections, not just declarations) — for the single dependency package a
// caller currently has a file open in, mirroring gopls's own treatment of a
// "syntax target" versus an import-only dependency (see the package doc's
// Q2 reference: checkPackage vs. checkPackageForImport). Held in a separate,
// smaller LRU than Package's own (FullBodyDefaultCap, not DefaultCap — see
// its doc), evicted independently.
//
// Import resolution shares identity with Package's own cache: this
// CheckedPackage's own imports, and any OTHER package's import of pkgPath
// (via Package or PackageWithBodies), both resolve through the same
// Provider and so land on this exact instance while it stays in the
// full-body LRU (importer.ImportFrom consults the full-body cache before
// the declarations-only one) — the mechanism that unifies identity across
// "a file opened directly inside this dependency" and "a jump target that
// happens to reference it," per the design this method exists for. Once
// evicted, a later Package/PackageWithBodies call for pkgPath re-checks it
// from scratch, producing a new, independent instance — no different from
// any other LRU eviction; see the package doc for why this bounded
// divergence is an accepted tradeoff rather than something golance's
// on-demand identity needs to solve for.
func (p *Provider) PackageWithBodies(ctx context.Context, pkgPath string) (*CheckedPackage, error) {
	scope := newClosureScope()
	defer scope.close(p)
	return p.packageWithBodiesScoped(ctx, pkgPath, scope)
}

// packageWithBodiesScoped is PackageWithBodies's own implementation,
// additionally threading scope through every nested resolution this call
// triggers — see packageScoped's identical doc, including for why nested
// callers must not add their own scope.close.
func (p *Provider) packageWithBodiesScoped(ctx context.Context, pkgPath string, scope *closureScope) (*CheckedPackage, error) {
	if cp, ok := scope.get(pkgPath); ok {
		return cp, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pkgPath == unsafePkgPath {
		return unsafePackage(), nil
	}
	if cp, ok := p.tryPinFull(pkgPath, scope); ok {
		return cp, nil
	}

	p.enterWaitFull(pkgPath)
	v, err, _ := p.fullSF.Do(pkgPath, func() (any, error) {
		if cp, ok := p.getFull(pkgPath); ok {
			return cp, nil
		}
		cp, err := p.check(ctx, pkgPath, true, scope)
		if err != nil {
			return nil, err
		}
		p.putFull(pkgPath, cp)
		return cp, nil
	})
	if err != nil {
		p.exitWaitFull(pkgPath)
		return nil, err
	}
	cp, ok := v.(*CheckedPackage)
	if !ok {
		p.exitWaitFull(pkgPath)
		return nil, fmt.Errorf("depcheck: singleflight for %s returned %T, want *CheckedPackage", pkgPath, v)
	}
	p.exitWaitFullAndPin(pkgPath, cp, scope)
	return cp, nil
}

// get returns pkgPath's cached declarations-only CheckedPackage, if the LRU
// currently holds one, bumping its recency. Only used inside the
// singleflight closure's own double-check (packageScoped's own miss path):
// every OTHER caller uses tryPin/tryPinFull or exitWaitAndPin/
// exitWaitFullAndPin instead, which look up AND pin atomically — see
// tryPin's own doc for why a separate get-then-pin here would reopen the
// exact eviction race those exist to close.
func (p *Provider) get(pkgPath string) (*CheckedPackage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lru.get(pkgPath)
}

// put stores cp in the declarations-only LRU under pkgPath (see
// lruCache.put's own doc), records that a fresh check happened (see
// Checked), and — if any caller is currently registered as a waiter for
// pkgPath (see enterWait) — gives the fresh entry a birth pin that keeps it
// alive until every registered waiter has retired via exitWait/
// exitWaitAndPin. Without this, the entry would sit unpinned in the LRU for
// the entire window between here and each singleflight-collapsed caller's
// own, separately-locked pin call, during which any unrelated concurrent
// put's own evictOldest could remove it.
func (p *Provider) put(pkgPath string, cp *CheckedPackage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checked++
	p.lru.put(pkgPath, cp)
	if p.waiters[pkgPath] > 0 {
		p.lru.pin(pkgPath)
	}
}

// getFull returns pkgPath's cached full-body CheckedPackage, if the
// full-body LRU currently holds one, bumping its recency — see get's own
// doc for why every caller besides the singleflight closure's own
// double-check uses an atomic tryPinFull/exitWaitFullAndPin instead.
func (p *Provider) getFull(pkgPath string) (*CheckedPackage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fullLRU.get(pkgPath)
}

// putFull stores cp in the full-body LRU under pkgPath and records that a
// fresh full-body check happened (see CheckedWithBodies) — see put's own
// doc for the birth-pin mechanism this mirrors, keyed against fullWaiters.
func (p *Provider) putFull(pkgPath string, cp *CheckedPackage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fullChecked++
	p.fullLRU.put(pkgPath, cp)
	if p.fullWaiters[pkgPath] > 0 {
		p.fullLRU.pin(pkgPath)
	}
}

// tryPinFull looks up pkgPath in the full-body LRU and, if present, pins it
// and records it into scope — atomically, under one lock acquisition. This
// must not be split into a separate lookup-then-pin pair of calls: between
// them, an unrelated concurrent put/putFull's own evictOldest could remove
// the very entry just "found," since nothing yet protects it in that gap
// (the bug TestProvider_LRU_PreventsCrossCallGenericTypeIdentitySplit and
// the real-GOMODCACHE Cap=8 regression this closes both reproduced once
// concurrency was high enough to make that gap land routinely rather than
// rarely).
func (p *Provider) tryPinFull(pkgPath string, scope *closureScope) (*CheckedPackage, bool) {
	p.mu.Lock()
	cp, ok := p.fullLRU.get(pkgPath)
	if !ok {
		p.mu.Unlock()
		return nil, false
	}
	p.fullLRU.pin(pkgPath)
	p.mu.Unlock()
	scope.put(pkgPath, cp, p.fullLRU)
	return cp, true
}

// tryPin looks up pkgPath in the declarations-only LRU and, if present,
// pins it and records it into scope — atomically; see tryPinFull's own doc
// for why this must not be split across two locked sections.
func (p *Provider) tryPin(pkgPath string, scope *closureScope) (*CheckedPackage, bool) {
	p.mu.Lock()
	cp, ok := p.lru.get(pkgPath)
	if !ok {
		p.mu.Unlock()
		return nil, false
	}
	p.lru.pin(pkgPath)
	p.mu.Unlock()
	scope.put(pkgPath, cp, p.lru)
	return cp, true
}

// enterWait registers the calling goroutine as a pending waiter for
// pkgPath's declarations-only singleflight check — called before entering
// p.sf.Do, by every caller (the eventual leader and every follower
// singleflight collapses onto it alike), so put (see its own doc) knows to
// give the entry it is about to insert a birth pin. Every enterWait call
// must be matched by exactly one later exitWait (on failure) or
// exitWaitAndPin (on success) call, from the same goroutine, after its own
// p.sf.Do call returns.
func (p *Provider) enterWait(pkgPath string) {
	p.mu.Lock()
	p.waiters[pkgPath]++
	p.mu.Unlock()
}

// exitWait retires this waiter's registration (see enterWait) without
// pinning — the check failed, so put never ran and no entry exists to
// protect.
func (p *Provider) exitWait(pkgPath string) {
	p.mu.Lock()
	p.retireWaiterLocked(pkgPath)
	p.mu.Unlock()
}

// exitWaitAndPin retires this waiter's registration and pins pkgPath into
// scope on this waiter's own behalf, atomically in one locked section — so
// the entry is never left unpinned between "put gave it a birth pin" and
// "every waiter has recorded its own durable pin." Releases the birth pin
// once the last registered waiter retires (see retireWaiterLocked).
func (p *Provider) exitWaitAndPin(pkgPath string, cp *CheckedPackage, scope *closureScope) {
	p.mu.Lock()
	p.lru.pin(pkgPath)
	p.retireWaiterLocked(pkgPath)
	p.mu.Unlock()
	scope.put(pkgPath, cp, p.lru)
}

// retireWaiterLocked decrements waiters[pkgPath], releasing the birth pin
// put gave the entry once the count reaches zero (no registered waiter
// remains that still needs it protected). p.mu must be held.
func (p *Provider) retireWaiterLocked(pkgPath string) {
	if n := p.waiters[pkgPath] - 1; n > 0 {
		p.waiters[pkgPath] = n
		return
	}
	delete(p.waiters, pkgPath)
	p.lru.unpin(pkgPath)
}

// enterWaitFull, exitWaitFull, and exitWaitFullAndPin mirror enterWait/
// exitWait/exitWaitAndPin for the full-body LRU/fullSF — see their docs.
func (p *Provider) enterWaitFull(pkgPath string) {
	p.mu.Lock()
	p.fullWaiters[pkgPath]++
	p.mu.Unlock()
}

func (p *Provider) exitWaitFull(pkgPath string) {
	p.mu.Lock()
	p.retireWaiterFullLocked(pkgPath)
	p.mu.Unlock()
}

func (p *Provider) exitWaitFullAndPin(pkgPath string, cp *CheckedPackage, scope *closureScope) {
	p.mu.Lock()
	p.fullLRU.pin(pkgPath)
	p.retireWaiterFullLocked(pkgPath)
	p.mu.Unlock()
	scope.put(pkgPath, cp, p.fullLRU)
}

func (p *Provider) retireWaiterFullLocked(pkgPath string) {
	if n := p.fullWaiters[pkgPath] - 1; n > 0 {
		p.fullWaiters[pkgPath] = n
		return
	}
	delete(p.fullWaiters, pkgPath)
	p.fullLRU.unpin(pkgPath)
}

// Delete drops each of pkgPaths from both the declarations-only and
// full-body LRUs, if cached. Callers use this after a workspace package's
// on-disk export data changes (didSave's background reindex, see
// internal/server.Server.reindex): unlike a GOROOT/module-cache dependency,
// which Provider assumes is immutable for a fixed dependency set (see
// Provider's own doc), a workspace package is reachable through this same
// Provider too — depexport.Cache.ExportData falls back to Provider.Package
// for ANY pkgPath its MetadataSource resolves, including a workspace
// package imported by another workspace package, since only a
// GOROOT/module-cache directory is treated as immutable enough to persist
// to the CAS there — and nothing else in Provider notices that content
// changing underneath a cached entry.
func (p *Provider) Delete(pkgPaths ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pkgPath := range pkgPaths {
		p.lru.delete(pkgPath)
		p.fullLRU.delete(pkgPath)
	}
}

const unsafePkgPath = "unsafe"

// unsafePackage returns the synthetic CheckedPackage for the "unsafe"
// pseudo-package: types.Unsafe is a fixed, pre-built *types.Package with no
// source files of its own (mirrors gopls's own checkPackageForImport
// special case, research-gopls-dependency-nav.md's Q2).
func unsafePackage() *CheckedPackage {
	return &CheckedPackage{pkgPath: unsafePkgPath, pkg: types.Unsafe, info: &types.Info{}}
}

// check parses pkgPath's GoFiles (from metadata; disk content, since
// module-cache and GOROOT files are immutable) into p.fset and type-checks
// them, resolving pkgPath's own imports recursively through p itself (see
// importer). withBodies selects IgnoreFuncBodies: false — used only by
// PackageWithBodies, for the single package a caller has open — versus
// Package's own true (declarations only, the common case for resolving a
// jump target's signature/doc); doc comments come from parser.ParseComments
// regardless of that setting.
//
// scope is the calling packageScoped/packageWithBodiesScoped's own
// closureScope, carried into the ctxImporter this check builds so pkgPath's
// entire transitive closure — however deep the recursion goes — pins every
// distinct import path it touches to one *CheckedPackage for scope's whole
// lifetime (see closureScope's own doc).
func (p *Provider) check(ctx context.Context, pkgPath string, withBodies bool, scope *closureScope) (*CheckedPackage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, goFiles, _, ok := p.meta.Package(pkgPath)
	if !ok {
		return nil, fmt.Errorf("depcheck: %s is not known to the import graph", pkgPath)
	}
	if len(goFiles) == 0 {
		return nil, fmt.Errorf("depcheck: %s (%s) has no Go files", pkgPath, dir)
	}

	files := make([]*ast.File, 0, len(goFiles))
	for _, path := range goFiles {
		f, err := parser.ParseFile(p.fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("depcheck: parse %s: %w", path, err)
		}
		files = append(files, f)
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
	imp := &ctxImporter{p: p, ctx: ctx, scope: scope}
	var hadErr bool
	var firstErr string
	conf := types.Config{
		Importer:         imp,
		IgnoreFuncBodies: !withBodies,
		// best-effort: a dependency's own source is immutable and assumed to
		// compile; a type error here (including an unresolved transitive
		// import) degrades to a possibly-incomplete pkg rather than failing
		// the whole check. hadErr — folded into the returned
		// CheckedPackage.Incomplete — lets a caller that must not persist a
		// degraded result (internal/depexport) refuse to, without this
		// best-effort fallback itself changing. firstErr is a diagnostic
		// sample only (see CheckedPackage.FirstError's own doc).
		Error: func(err error) {
			hadErr = true
			if firstErr == "" {
				firstErr = err.Error()
			}
		},
	}
	pkg, _ := conf.Check(pkgPath, p.fset, files, info)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &CheckedPackage{pkgPath: pkgPath, dir: dir, files: files, pkg: pkg, info: info, incomplete: hadErr || imp.importIncomplete, firstError: firstErr}, nil
}

// ctxImporter implements types.ImporterFrom by resolving each import back
// through the same Provider, recursively — the "re-entrant check on demand"
// callback pattern gopls's own getImportPackage uses (see the package doc's
// Q2 reference), needed because an imported dependency can itself have been
// evicted from the LRU since it was last checked. Built fresh by check for
// each top-level check call (one small allocation on a cache/singleflight
// miss — the only path that reaches check at all) rather than being a cast
// of *Provider itself, specifically so it can carry that call's own ctx
// through every recursive import: without this, a huge dependency closure
// (a large monorepo's shared internal package, say) had no way to notice
// its caller gave up partway through — types.Config.Check invokes
// ImportFrom synchronously, once per import, as it walks the package's own
// import list, so checking ctx.Err() at the top of ImportFrom is a
// checkpoint between each package the recursive descent is about to check
// next, not just at the very start and end of the top-level call.
//
// Fairness caveat: a check already shared via singleflight (see
// Package/PackageWithBodies) uses only the LEADER's ctx, so if the leader's
// own request is canceled but a follower's is not, this can still fail the
// follower's call too — the same tradeoff the pre-existing ctx.Err() checks
// in check/Package already made (a canceled leader could already leave a
// follower waiting on a check that keeps running to completion regardless);
// this only makes that check happen sooner, so an abandoned closure check
// stops burning CPU promptly instead of only at the very end.
type ctxImporter struct {
	p     *Provider
	ctx   context.Context
	scope *closureScope

	// importIncomplete is set once ImportFrom resolves an import that is
	// itself Incomplete, so the check currently underway inherits that —
	// see CheckedPackage.Incomplete's doc for why this propagation is
	// needed rather than relying on types.Config.Error alone. Read only
	// after conf.Check (in check) returns, from the same goroutine that
	// built imp and drove that call — types.Config.Check invokes
	// ImportFrom synchronously (see this type's own doc), so no
	// synchronization is needed for this field.
	importIncomplete bool
}

// closureScope pins every distinct import path one top-level
// Provider.Package/PackageWithBodies call's own recursive resolution
// resolves, to the exact *CheckedPackage it first got back, for TWO
// overlapping durations:
//
//   - IN-FLIGHT, for this one call's own entire duration (released by
//     close, called once packageScoped/packageWithBodiesScoped returns):
//     protects whatever this closure is actively resolving right now, even
//     before anything else has cached a reference to it.
//   - CACHED-LIFETIME, for as long as whichever entry FIRST cached each
//     resolved package stays in p's own LRU (see Provider.get/put's own
//     doc, and lruCache.put's — pinned atomically with the lookup/insert
//     that produced it, in the identical *Provider.mu critical section, so
//     no separate, later pin call ever leaves a race window open): protects
//     a dependency for as long as some OTHER, already-finished closure's
//     own cached result still embeds it, well past this closure's own
//     lifetime.
//
// Both pin sources contribute to the identical lruCache.pins refcount (see
// lruCache's own doc), so an entry survives as long as either one still
// needs it — this is the fix for the KNOWN GAP an earlier revision of this
// doc described: a cache HIT (p.get/p.getFull) handing closure B an
// already-cached CheckedPackage P that closure A produced, whose own live
// *types.Package graph embeds a reference to some dependency D, used to
// leave D free to be evicted and re-checked (a different generation)
// before B asked for D directly — confirmed reproducible
// (github.com/aws/aws-sdk-go-v2/internal/auth/smithy and
// .../service/s3/internal/customizations, against a real, GOMODCACHE-
// resident dependency graph, under Cap sized far below the closure's own
// size) as a genuine go/types interface-satisfaction error ("does not
// implement ... wrong type for method ..."). P's own cached-lifetime pin on
// D (established the moment P was first cached, not merely while some
// closure is actively resolving it) now keeps D alive for exactly as long
// as P itself stays cached, regardless of whether the closure that first
// produced P has long since returned.
//
// Without EITHER half, p's shared declarations-only LRU (sized for
// cross-closure reuse, not for holding one closure's full working set — see
// DefaultCap/RecommendedCap's own docs, which already acknowledge "a single
// dependency closure larger than a small cap DOES thrash it") can evict a
// widely-shared package mid-resolution, so a later ImportFrom for the
// identical import path — within the SAME closure (the in-flight case), or
// from a DIFFERENT one reusing an already-cached result that embeds it (the
// cached-lifetime case) — decodes a second, non-identical *types.Package
// for what is declaration-for-declaration identical source. go/types
// compares named types (and satisfies generic instantiations) by object
// identity, not structural shape, so the two non-identical instances
// feeding into one Checker.Check call make an otherwise valid generic
// instantiation fail an interface-satisfaction check that would pass under
// `go build` — the "widely-shared dependency gets evicted and re-checked
// from scratch partway through resolving cp's own transitive imports"
// mechanism internal/depexport's own checkAndPersist doc already names,
// confirmed reproducible without any export-data round trip at all (see
// internal/depcheck's own genericsplit_test.go).
//
// A closureScope is created fresh for every top-level Package/
// PackageWithBodies call; its own IN-FLIGHT pins are released once that
// call returns (see close), but its CACHED-LIFETIME pins (established via
// Provider.put/putFull, not by the closureScope itself) persist for as long
// as the entry they protect stays in p's own LRU — chaining one level of
// direct pins per cached entry (an entry pins its own cp.Types().Imports(),
// which themselves pin THEIR OWN Imports() for as long as THEY stay cached,
// and so on) is sufficient to protect a package's full transitive closure
// without any entry needing to compute or store it.
//
// Deliberately does not extend to imp.p's exportResolver (the decode fast
// path — see its own doc): a decoded package there lives in its own
// dedicated fset/cache, is never a Decl/DeclAt target, and never mixes with
// a p.fset-based CheckedPackage in the first place, so it needs no
// closure-local pinning of its own; exportDecodeCap's coarse whole-cache
// reset is that path's own, pre-existing, separately-documented answer to
// the identical problem, left untouched here.
type closureScope struct {
	mu   sync.Mutex
	pkgs map[string]*CheckedPackage
	// pins records, per resolved path, which lruCache its IN-FLIGHT pin
	// (see Provider.get/getFull/put/putFull's own atomic pinning) lives in
	// — nil for "unsafe" (see unsafePackage's own doc), the one
	// CheckedPackage this scope never actually resolves through either LRU
	// at all, so there is nothing to release for it.
	pins map[string]*lruCache
}

// newClosureScope returns an empty closureScope.
func newClosureScope() *closureScope {
	return &closureScope{pkgs: make(map[string]*CheckedPackage), pins: make(map[string]*lruCache)}
}

// get returns pkgPath's pinned CheckedPackage, if s already holds one.
func (s *closureScope) get(pkgPath string) (*CheckedPackage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.pkgs[pkgPath]
	return cp, ok
}

// put records cp under pkgPath for the rest of s's lifetime, along with
// which lruCache pkgPath's own IN-FLIGHT pin (already established
// atomically by whichever Provider.get/getFull/put/putFull call actually
// produced cp — see their own docs) lives in, for close to release later.
// Never overwrites an existing entry: every caller either already checked
// get first, or resolves pkgPath via the same singleflight-guarded path
// that makes every concurrent caller for one pkgPath observe the identical
// cp regardless of which of them calls put first — lru is nil for
// "unsafe" (see unsafePackage's own doc).
func (s *closureScope) put(pkgPath string, cp *CheckedPackage, lru *lruCache) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pkgs[pkgPath]; ok {
		return
	}
	s.pkgs[pkgPath] = cp
	s.pins[pkgPath] = lru
}

// close releases every IN-FLIGHT pin s acquired over its lifetime (see
// closureScope's own doc — this does NOT touch any CACHED-LIFETIME pin
// Provider.put/putFull established; those live and die with whichever
// cache entry they protect, independent of any one closureScope). Call
// exactly once, after the top-level Package/PackageWithBodies call s was
// created for has returned. p.mu must not be held by the caller.
func (s *closureScope) close(p *Provider) {
	s.mu.Lock()
	pins := s.pins
	s.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for path, lru := range pins {
		if lru != nil {
			lru.unpin(path)
		}
	}
}

func (imp *ctxImporter) Import(path string) (*types.Package, error) {
	return imp.ImportFrom(path, "", 0)
}

// ImportFrom resolves path against imp's own closureScope first (see its
// doc), so a path already resolved earlier in THIS SAME top-level check's
// transitive closure always reuses that exact instance regardless of
// whether p's shared LRU has since evicted it for an unrelated, concurrently
// in-flight closure. Only on a scope miss does this fall to the full-body
// LRU next, so an import of a package the caller also has open (via
// PackageWithBodies) shares its exact *types.Package identity instead of
// triggering a second, divergent declarations-only check — see
// PackageWithBodies's doc. Next, the declarations-only LRU: an
// already-resident, source-checked instance is always preferred over a
// decode, since it costs nothing further to reuse. Only once all three miss
// does this fall to imp.p's exportResolver (see its own doc), decoding path
// from persisted export data instead of a full recursive source-check — the
// fix for the production regression measured against a dependency closure
// larger than the LRU's own cap: without an ExportSource configured
// (imp.p.exportResolverFor returns nil), this falls straight through to the
// original full-check behavior, unchanged.
//
// "unsafe" is special-cased before any of that, exactly like
// Provider.Package's own identical check (and
// typecheck.Importer.ImportFrom's): types.Unsafe has no source and
// gcexportdata.Write panics unconditionally trying to serialize it (see
// depexport.Cache.ExportData's doc), so the exportResolver path — which
// would otherwise ask an ExportSource for "unsafe" bytes exactly like any
// other transitive import — must never see it.
func (imp *ctxImporter) ImportFrom(path, _ string, _ types.ImportMode) (*types.Package, error) {
	if err := imp.ctx.Err(); err != nil {
		return nil, err
	}
	if path == unsafePkgPath {
		return types.Unsafe, nil
	}
	if cp, ok := imp.scope.get(path); ok {
		imp.importIncomplete = imp.importIncomplete || cp.Incomplete()
		return cp.Types(), nil
	}
	if cp, ok := imp.p.tryPinFull(path, imp.scope); ok {
		imp.importIncomplete = imp.importIncomplete || cp.Incomplete()
		return cp.Types(), nil
	}
	if cp, ok := imp.p.tryPin(path, imp.scope); ok {
		imp.importIncomplete = imp.importIncomplete || cp.Incomplete()
		return cp.Types(), nil
	}
	if r := imp.p.exportResolverFor(); r != nil {
		pkg, complete, ok, err := r.resolve(imp.ctx, path)
		if err != nil {
			return nil, err
		}
		if ok {
			imp.importIncomplete = imp.importIncomplete || !complete
			imp.p.recordDecoded()
			return pkg, nil
		}
	}
	cp, err := imp.p.packageScoped(imp.ctx, path, imp.scope)
	if err != nil {
		return nil, err
	}
	imp.importIncomplete = imp.importIncomplete || cp.Incomplete()
	return cp.Types(), nil
}

// Decl locates obj's declaring identifier inside pkgPath's source-checked
// package (resolving and checking pkgPath on demand via Package), returning
// the identifier and the Provider's shared FileSet its position is valid
// against.
//
// obj is expected to have been resolved against some OTHER *types.Package
// instance for pkgPath — typically one decoded from compiler export data,
// as every current DependencyDefinition caller does, since a workspace
// file can only ever reference an EXPORTED dependency symbol in the first
// place (Go's own visibility rule) — not against the *types.Package this
// Provider itself produces; mapping across that instance boundary is
// exactly what Decl exists for:
//
//   - Exported objects (the only kind reachable from workspace code) are
//     mapped via golang.org/x/tools/go/types/objectpath: a structural,
//     decode-source-independent encoding of an object's position in its
//     package's API surface (e.g. "TypeName.Method"), computed once from
//     obj and decoded against p's own checked package.
//   - If that fails — obj is unexported, e.g. a jump originating INSIDE a
//     dependency file itself (a later phase's use case, not this phase's
//     wired consumer, but supported here since it costs nothing extra) —
//     Decl falls back to a package-level scope lookup by name. This
//     resolves a plain package-level unexported declaration but not an
//     unexported method or a name shadowed at a non-package scope; wiring
//     that case robustly needs the caller's own AST position (as
//     internal/langfeat.SamePackageDefinition does for a workspace file),
//     not just a name, and is left to whichever later consumer needs it.
func (p *Provider) Decl(ctx context.Context, pkgPath string, obj types.Object) (*ast.Ident, *token.FileSet, error) {
	cp, err := p.Package(ctx, pkgPath)
	if err != nil {
		return nil, nil, err
	}
	target, err := resolveObject(cp, obj)
	if err != nil {
		return nil, nil, err
	}
	return p.declOf(cp, target)
}

// DeclAt locates the declaring identifier of the object identified by
// (pkgPath, objPath) — an objectpath.Path string, in the identical format
// [golang.org/x/tools/go/types/objectpath] produces and
// internal/store.BuildSymbolID's own SymbolID encoding embeds — inside
// pkgPath's source-checked package (resolving and checking it on demand via
// Package). Unlike Decl, this needs no live types.Object from the caller's
// own, separately-resolved *types.Package instance: a caller that already
// computed objPath itself (e.g. internal/langfeat.TypeDefinition, whose
// cross-package result already carries objPath for the workspace facts
// index's own [xref.Resolver.TypeDeclaration] — this is that same lookup's
// fallback when the target is a dependency the facts index does not cover)
// can resolve straight from the string, without needing to keep a
// cross-instance types.Object alive just to re-derive it. ok is false, with
// an error, if objPath is not a valid encoding or does not resolve against
// pkgPath's package (e.g. it names something unexported that objectpath
// itself never encodes — see resolveObject's identical fallback for that
// case via Decl instead).
func (p *Provider) DeclAt(ctx context.Context, pkgPath, objPath string) (*ast.Ident, *token.FileSet, error) {
	cp, err := p.Package(ctx, pkgPath)
	if err != nil {
		return nil, nil, err
	}
	target, err := objectpath.Object(cp.pkg, objectpath.Path(objPath))
	if err != nil {
		return nil, nil, fmt.Errorf("depcheck: resolve %s in %s: %w", objPath, pkgPath, err)
	}
	return p.declOf(cp, target)
}

// declOf returns target's declaring identifier among cp's own parsed files,
// the shared work Decl and DeclAt both need once they have a resolved
// types.Object in cp's own *types.Package instance.
func (p *Provider) declOf(cp *CheckedPackage, target types.Object) (*ast.Ident, *token.FileSet, error) {
	if !target.Pos().IsValid() {
		return nil, nil, fmt.Errorf("depcheck: %s has no valid declaration position in %s", target.Name(), cp.pkgPath)
	}
	id := declIdent(cp.files, p.fset, target.Pos())
	if id == nil {
		return nil, nil, fmt.Errorf("depcheck: no declaring identifier found for %s in %s", target.Name(), cp.pkgPath)
	}
	return id, p.fset, nil
}

// DocAt returns the doc comment recorded for the object identified by
// (pkgPath, objPath) — the identical objectpath encoding DeclAt takes (see
// its doc) — inside pkgPath's source-checked package. Unlike a workspace
// package's facts index, which only ever records a doc comment for a
// declaration it indexed as a root package's own unit (see
// [xref.Resolver.SymbolDoc]'s identical doc-only lookup), this reads
// straight from real, ParseComments-parsed source: hover/completion-doc
// into a dependency (strings.Builder, testing.T, ...) shows its actual doc
// comment instead of nothing, gopls's own hover content for the same
// symbol (declaration + doc comment). "" if objPath resolves to an object
// with no doc comment; an error only if pkgPath or objPath itself fails to
// resolve at all.
func (p *Provider) DocAt(ctx context.Context, pkgPath, objPath string) (string, error) {
	cp, err := p.Package(ctx, pkgPath)
	if err != nil {
		return "", err
	}
	target, err := objectpath.Object(cp.pkg, objectpath.Path(objPath))
	if err != nil {
		return "", fmt.Errorf("depcheck: resolve %s in %s: %w", objPath, pkgPath, err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !target.Pos().IsValid() {
		return "", nil
	}
	return docAt(cp.files, p.fset, target.Pos()), nil
}

// docAt returns the doc comment attached to the declaration at pos among
// files ("" if none, or if pos falls in none of them), mirroring
// internal/langfeat.docForObject's identical AST walk for a same-package
// object — duplicated here rather than shared across the package boundary,
// since CheckedPackage's own fields are deliberately unexported (see its
// immutability doc) and langfeat cannot reach into them directly.
func docAt(files []*ast.File, fset *token.FileSet, pos token.Pos) string {
	tf := fset.File(pos)
	if tf == nil {
		return ""
	}
	var declFile *ast.File
	for _, f := range files {
		if fset.File(f.Pos()) == tf {
			declFile = f
			break
		}
	}
	if declFile == nil {
		return ""
	}
	path, _ := astutil.PathEnclosingInterval(declFile, pos, pos)
	for _, n := range path {
		switch d := n.(type) {
		case *ast.FuncDecl:
			return d.Doc.Text()
		case *ast.TypeSpec:
			if d.Doc != nil {
				return d.Doc.Text()
			}
		case *ast.ValueSpec:
			if d.Doc != nil {
				return d.Doc.Text()
			}
		case *ast.Field:
			if d.Doc != nil {
				return d.Doc.Text()
			}
		case *ast.GenDecl:
			return d.Doc.Text()
		}
	}
	return ""
}

// resolveObject maps obj — resolved against some other *types.Package
// instance for cp's own PkgPath — onto the equivalent types.Object inside
// cp.Types(). See Decl's doc for the two strategies tried.
func resolveObject(cp *CheckedPackage, obj types.Object) (types.Object, error) {
	obj = OriginObject(obj)
	if path, err := objectpath.For(obj); err == nil {
		if target, err := objectpath.Object(cp.pkg, path); err == nil {
			return target, nil
		}
	}
	if target := cp.pkg.Scope().Lookup(obj.Name()); target != nil {
		return target, nil
	}
	return nil, fmt.Errorf("depcheck: could not resolve %s in %s", obj.Name(), cp.pkgPath)
}

// OriginObject normalizes obj to the declaration objectpath.For can encode a
// path for, when obj is a synthetic object go/types created while
// instantiating a generic type: a field or method reached through an
// instantiated generic type (e.g. connect.Request[T].Msg, or a method on
// Box[Concrete]) is a distinct *types.Var/*types.Func from its origin
// declaration — not identity-equal to it, even though both live in the same
// *types.Package — so objectpath.For's traversal, which only walks origin
// declarations reachable from package scope, cannot find a path for the
// synthetic one directly ("can't find path" from
// golang.org/x/tools/go/types/objectpath). types.Var and types.Func both
// expose Origin() for exactly this: it returns the receiver unchanged for
// every object that is not itself such a synthetic instantiation artifact,
// so calling it unconditionally here is a no-op for the common, non-generic
// case, and for a promoted field/method reached through an embedded
// instantiated generic type (the same synthetic object is returned by
// go/types either way). No equivalent normalization is needed for
// *types.TypeName or a plain generic function's *types.Func: go/types
// creates exactly one object per declaration for those — instantiating a
// named type or a generic function produces a new go/types.Type or
// types.Instance, never a second Var/Func/TypeName — so an identifier
// referring to either already resolves to the origin object without help.
//
// Exported for internal/langfeat's own direct objectpath.For call sites
// (hover, completion-doc, call hierarchy): resolveObject above needs it to
// bridge Decl's cross-instance *types.Package boundary, but those callers
// hit the identical "can't find path" obstacle earlier, encoding an
// objectpath straight from a live obj resolved against cp's own Info,
// before any depcheck.Provider round-trip -- the same normalization applies
// either way.
func OriginObject(obj types.Object) types.Object {
	switch o := obj.(type) {
	case *types.Var:
		return o.Origin()
	case *types.Func:
		return o.Origin()
	default:
		return obj
	}
}

// declIdent returns the *ast.Ident at pos among files — the declaring
// identifier itself, since pos is always a declaration's own Pos() — or nil
// if pos falls in none of them (should not happen for a pos this Provider's
// own check produced, but guarded rather than assumed).
func declIdent(files []*ast.File, fset *token.FileSet, pos token.Pos) *ast.Ident {
	tf := fset.File(pos)
	if tf == nil {
		return nil
	}
	for _, f := range files {
		if fset.File(f.Pos()) != tf {
			continue
		}
		path, _ := astutil.PathEnclosingInterval(f, pos, pos)
		for _, n := range path {
			if id, ok := n.(*ast.Ident); ok {
				return id
			}
		}
	}
	return nil
}
