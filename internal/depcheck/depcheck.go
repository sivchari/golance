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
func (g GraphMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	pkg, ok := g.snap.Package(pkgPath)
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
	pkgPath string
	dir     string
	files   []*ast.File
	pkg     *types.Package
	info    *types.Info
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

// RecommendedCap returns the LRU capacity a Provider dedicated to BATCH
// export-data production (internal/depexport's use — not interactive
// navigation, which should keep DefaultCap) should be constructed with,
// given nonRootCount: the number of non-root (stdlib/module-cache)
// packages the run may need to resolve, transitively. Sized to hold every
// one of them live for the run's whole duration — never DefaultCap, which
// exists for an entirely different, much smaller-locality workload (see
// DefaultCap's own doc) and thrashes badly at this scale: a real
// measurement against a synthetic ~370-package dependency closure went
// from ~7s/~300MB (a correctly-sized cap) to ~72s/~7GB (DefaultCap) purely
// from LRU eviction forcing the same widely-shared packages to be
// rechecked from scratch over and over. DefaultCap is still used as a
// floor for a tiny workspace's few dependencies, matching Provider's own
// existing default for that case.
func RecommendedCap(nonRootCount int) int {
	if nonRootCount < DefaultCap {
		return DefaultCap
	}
	return nonRootCount
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
	fullLRU     *lruCache // full-body-checked packages (see PackageWithBodies), separate from lru so a small handful of open dependency files never evicts the much larger decl-only working set
	checked     int64     // count of Package calls that actually ran CheckPackage (cache+singleflight misses); test/observability hook.
	fullChecked int64     // count of PackageWithBodies calls that actually ran a fresh full-body check; test/observability hook.
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
func (p *Provider) Package(ctx context.Context, pkgPath string) (*CheckedPackage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pkgPath == unsafePkgPath {
		return unsafePackage(), nil
	}
	if cp, ok := p.getFull(pkgPath); ok {
		return cp, nil
	}
	if cp, ok := p.get(pkgPath); ok {
		return cp, nil
	}

	v, err, _ := p.sf.Do(pkgPath, func() (any, error) {
		if cp, ok := p.getFull(pkgPath); ok {
			return cp, nil
		}
		if cp, ok := p.get(pkgPath); ok {
			return cp, nil
		}
		cp, err := p.check(ctx, pkgPath, false)
		if err != nil {
			return nil, err
		}
		p.put(pkgPath, cp)
		return cp, nil
	})
	if err != nil {
		return nil, err
	}
	cp, ok := v.(*CheckedPackage)
	if !ok {
		return nil, fmt.Errorf("depcheck: singleflight for %s returned %T, want *CheckedPackage", pkgPath, v)
	}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pkgPath == unsafePkgPath {
		return unsafePackage(), nil
	}
	if cp, ok := p.getFull(pkgPath); ok {
		return cp, nil
	}

	v, err, _ := p.fullSF.Do(pkgPath, func() (any, error) {
		if cp, ok := p.getFull(pkgPath); ok {
			return cp, nil
		}
		cp, err := p.check(ctx, pkgPath, true)
		if err != nil {
			return nil, err
		}
		p.putFull(pkgPath, cp)
		return cp, nil
	})
	if err != nil {
		return nil, err
	}
	cp, ok := v.(*CheckedPackage)
	if !ok {
		return nil, fmt.Errorf("depcheck: singleflight for %s returned %T, want *CheckedPackage", pkgPath, v)
	}
	return cp, nil
}

// get returns pkgPath's cached declarations-only CheckedPackage, if the LRU
// currently holds one, bumping its recency.
func (p *Provider) get(pkgPath string) (*CheckedPackage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lru.get(pkgPath)
}

// put stores cp in the declarations-only LRU under pkgPath, evicting the
// least recently used entry first if the LRU is at capacity, and records
// that a fresh check happened (see Checked).
func (p *Provider) put(pkgPath string, cp *CheckedPackage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checked++
	p.lru.put(pkgPath, cp)
}

// getFull returns pkgPath's cached full-body CheckedPackage, if the
// full-body LRU currently holds one, bumping its recency.
func (p *Provider) getFull(pkgPath string) (*CheckedPackage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fullLRU.get(pkgPath)
}

// putFull stores cp in the full-body LRU under pkgPath, evicting the least
// recently used entry first if the LRU is at capacity, and records that a
// fresh full-body check happened (see CheckedWithBodies).
func (p *Provider) putFull(pkgPath string, cp *CheckedPackage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fullChecked++
	p.fullLRU.put(pkgPath, cp)
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
func (p *Provider) check(ctx context.Context, pkgPath string, withBodies bool) (*CheckedPackage, error) {
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
	conf := types.Config{
		Importer:         &ctxImporter{p: p, ctx: ctx},
		IgnoreFuncBodies: !withBodies,
		Error:            func(error) {}, // best-effort: a dependency's own source is immutable and assumed to compile; a type error here degrades to a possibly-incomplete pkg rather than failing the whole check.
	}
	pkg, _ := conf.Check(pkgPath, p.fset, files, info)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &CheckedPackage{pkgPath: pkgPath, dir: dir, files: files, pkg: pkg, info: info}, nil
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
	p   *Provider
	ctx context.Context
}

func (imp *ctxImporter) Import(path string) (*types.Package, error) {
	return imp.ImportFrom(path, "", 0)
}

// ImportFrom resolves path against the full-body LRU first, so an import of
// a package the caller also has open (via PackageWithBodies) shares its
// exact *types.Package identity instead of triggering a second, divergent
// declarations-only check — see PackageWithBodies's doc.
func (imp *ctxImporter) ImportFrom(path, _ string, _ types.ImportMode) (*types.Package, error) {
	if err := imp.ctx.Err(); err != nil {
		return nil, err
	}
	if cp, ok := imp.p.getFull(path); ok {
		return cp.Types(), nil
	}
	cp, err := imp.p.Package(imp.ctx, path)
	if err != nil {
		return nil, err
	}
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
