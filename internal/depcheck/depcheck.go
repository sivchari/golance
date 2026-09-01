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
// tradeoff here). Bodies are never type-checked (types.Config.IgnoreFuncBodies
// is always true — see Provider's doc): navigation only needs declarations,
// and doc comments come from the AST (parser.ParseComments), present
// regardless of whether bodies are checked. Immutable once returned by
// Provider.Package, so sharing one instance across concurrent callers (via
// the LRU and singleflight) needs no further synchronization.
type CheckedPackage struct {
	pkgPath string
	files   []*ast.File
	pkg     *types.Package
	info    *types.Info
}

// PkgPath returns the package's import path.
func (cp *CheckedPackage) PkgPath() string { return cp.pkgPath }

// Files returns the package's parsed files, positions resolved against the
// owning Provider's FileSet.
func (cp *CheckedPackage) Files() []*ast.File { return cp.files }

// Types returns the checked *types.Package.
func (cp *CheckedPackage) Types() *types.Package { return cp.pkg }

// Info returns the *types.Info populated by the check (Defs, Uses,
// Selections, Types, Scopes, Instances, Implicits) — statement-level detail
// inside function bodies is absent (IgnoreFuncBodies), but every
// declaration is fully resolved.
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
const DefaultCap = 64

// Options configures a Provider.
type Options struct {
	// Cap is the LRU's entry capacity. Defaults to DefaultCap when <= 0.
	Cap int
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
	meta     MetadataSource
	capacity int

	fset *token.FileSet

	sf singleflight.Group

	mu      sync.Mutex
	lru     *lruCache
	checked int64 // count of Package calls that actually ran CheckPackage (cache+singleflight misses); test/observability hook.
}

// NewProvider returns a Provider resolving package metadata via meta,
// bounding its in-memory LRU at opts.Cap entries (DefaultCap if <= 0).
func NewProvider(meta MetadataSource, opts Options) *Provider {
	capacity := opts.Cap
	if capacity <= 0 {
		capacity = DefaultCap
	}
	return &Provider{meta: meta, capacity: capacity, fset: token.NewFileSet(), lru: newLRUCache(capacity)}
}

// FileSet returns the *token.FileSet every *CheckedPackage this Provider
// has ever returned shares (see Provider's doc). Positions read from any of
// them remain valid against this same *token.FileSet for the Provider's
// entire lifetime.
func (p *Provider) FileSet() *token.FileSet { return p.fset }

// Len returns the number of packages currently held in the LRU.
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

// Package returns pkgPath's source-type-checked CheckedPackage, checking it
// on demand if not already cached. Concurrent calls for the same pkgPath
// collapse onto a single check (singleflight); calls for different
// pkgPaths run independently and in parallel.
func (p *Provider) Package(ctx context.Context, pkgPath string) (*CheckedPackage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pkgPath == "unsafe" {
		return unsafePackage(), nil
	}
	if cp, ok := p.get(pkgPath); ok {
		return cp, nil
	}

	v, err, _ := p.sf.Do(pkgPath, func() (any, error) {
		if cp, ok := p.get(pkgPath); ok {
			return cp, nil
		}
		cp, err := p.check(ctx, pkgPath)
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

// get returns pkgPath's cached CheckedPackage, if the LRU currently holds
// one, bumping its recency.
func (p *Provider) get(pkgPath string) (*CheckedPackage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lru.get(pkgPath)
}

// put stores cp in the LRU under pkgPath, evicting the least recently used
// entry first if the LRU is at capacity, and records that a fresh check
// happened (see Checked).
func (p *Provider) put(pkgPath string, cp *CheckedPackage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checked++
	p.lru.put(pkgPath, cp)
}

// unsafePackage returns the synthetic CheckedPackage for the "unsafe"
// pseudo-package: types.Unsafe is a fixed, pre-built *types.Package with no
// source files of its own (mirrors gopls's own checkPackageForImport
// special case, research-gopls-dependency-nav.md's Q2).
func unsafePackage() *CheckedPackage {
	return &CheckedPackage{pkgPath: "unsafe", pkg: types.Unsafe, info: &types.Info{}}
}

// check parses pkgPath's GoFiles (from metadata; disk content, since
// module-cache and GOROOT files are immutable) into p.fset and
// type-checks them, resolving pkgPath's own imports recursively through p
// itself (see importer). Bodies are never checked (IgnoreFuncBodies):
// navigation needs declarations, not statement-level detail, and doc
// comments — needed for hover — come from parser.ParseComments regardless
// of that setting.
func (p *Provider) check(ctx context.Context, pkgPath string) (*CheckedPackage, error) {
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
		Importer:         (*importer)(p),
		IgnoreFuncBodies: true,
		Error:            func(error) {}, // best-effort: a dependency's own source is immutable and assumed to compile; a type error here degrades to a possibly-incomplete pkg rather than failing the whole check.
	}
	pkg, _ := conf.Check(pkgPath, p.fset, files, info)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &CheckedPackage{pkgPath: pkgPath, files: files, pkg: pkg, info: info}, nil
}

// importer implements types.ImporterFrom by resolving each import back
// through the same Provider, recursively — the "re-entrant check on
// demand" callback pattern gopls's own getImportPackage uses (see the
// package doc's Q2 reference), needed because an imported dependency can
// itself have been evicted from the LRU since it was last checked.
type importer Provider

func (imp *importer) Import(path string) (*types.Package, error) {
	return imp.ImportFrom(path, "", 0)
}

func (imp *importer) ImportFrom(path, _ string, _ types.ImportMode) (*types.Package, error) {
	cp, err := (*Provider)(imp).Package(context.Background(), path)
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
	if !target.Pos().IsValid() {
		return nil, nil, fmt.Errorf("depcheck: %s has no valid declaration position in %s", target.Name(), pkgPath)
	}
	id := declIdent(cp.files, p.fset, target.Pos())
	if id == nil {
		return nil, nil, fmt.Errorf("depcheck: no declaring identifier found for %s in %s", target.Name(), pkgPath)
	}
	return id, p.fset, nil
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
