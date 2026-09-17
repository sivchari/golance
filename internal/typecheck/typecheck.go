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
// Cache itself is append-only for as long as it is shared across concurrent
// checks: nothing here evicts an entry to bound memory. gcexportdata's
// decode can never produce a second, non-identical *types.Package for a
// path already present in Cache.pkgs — an incomplete placeholder is filled
// in and completed in place, a complete entry is returned as-is — so as
// long as no entry is ever removed while some other decode may still
// reference it, package identity is guaranteed by construction. Bounding
// memory is therefore the owner's job, not Cache's: once Bytes() exceeds
// some budget, discard this Cache and its fset together and start a fresh
// pair for new work — the same whole-pair-swap pattern internal/server's
// depCacheHolder, internal/xref's Resolver, and internal/depcheck's
// exportResolver already use for their own caches (see each package's own
// doc), and internal/index's own generations type applies to Build/Reindex.
// A caller that instead needs to invalidate a known set of paths without
// discarding everything else uses Invalidate, which returns a new Cache
// rather than mutating this one, preserving the same guarantee.
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
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{
		pkgs:   make(map[string]*types.Package),
		sizes:  make(map[string]int64),
		failed: make(map[string]error),
	}
}

// Invalidate returns a new Cache for callers that must treat changed's
// paths as stale (e.g. Resolver.Invalidate, depCacheHolder.invalidate,
// after a reindex changes their export data), sharing every surviving
// entry — not the underlying map — with c. It drops:
//
//   - every path in changed itself, and any cached ReadExport failure for
//     it, so a later decode re-reads fresh export data instead of serving
//     a stale success or a stale failure;
//   - every COMPLETE entry whose Imports() contains a path in changed: a
//     decoded, complete package's Imports() is the deep export data's own
//     full manifest, i.e. every package its declarations can reference
//     (see x/tools/internal/gcimporter/iimport.go's
//     SetImports(pkgList[1:])), so one-level membership is enough to find
//     every entry that could still embed the stale package;
//   - every INCOMPLETE entry, regardless of changed: gcexportdata inserts
//     an incomplete placeholder for a manifest path it has not yet been
//     asked to decode as a top-level package, and other entries' own
//     declarations can be decoded straight into that placeholder's scope
//     without ever completing it — references invisible to Imports() and
//     so impossible to find by walking it. Dropping every incomplete entry
//     unconditionally is cheap (decode recreates it from data already
//     available) and closes that blind spot instead of trying to track it.
//
// The returned Cache MUST be paired with the SAME *token.FileSet as c: a
// surviving entry's positions were registered into that fset by decode and
// nowhere else. c itself is left untouched, so a check or query already
// holding it (e.g. one pinned via a caller's own context, mirroring
// internal/xref's pinExportCache) keeps a consistent, append-only view for
// its own duration. This is Cache's staleness counterpart to the
// byte-budget whole-pair swap its own doc describes: unlike that swap,
// which discards every entry, Invalidate keeps every entry changed does
// not implicate.
func (c *Cache) Invalidate(changed []string) *Cache {
	c.mu.Lock()
	defer c.mu.Unlock()

	stale := make(map[string]bool, len(changed))
	for _, p := range changed {
		stale[p] = true
	}

	next := &Cache{
		pkgs:   make(map[string]*types.Package, len(c.pkgs)),
		sizes:  make(map[string]int64, len(c.sizes)),
		failed: make(map[string]error, len(c.failed)),
	}
	for path, pkg := range c.pkgs {
		if stale[path] || !pkg.Complete() || embedsAny(pkg, stale) {
			continue
		}
		next.pkgs[path] = pkg
		if size, ok := c.sizes[path]; ok {
			next.sizes[path] = size
			next.bytes += size
		}
	}
	for path, err := range c.failed {
		if stale[path] {
			continue
		}
		next.failed[path] = err
	}
	next.decodes = c.decodes
	return next
}

// embedsAny reports whether pkg's manifest (Imports()) contains any path in
// stale — see Invalidate's doc for why one-level membership over a
// complete package's Imports() is sufficient.
func embedsAny(pkg *types.Package, stale map[string]bool) bool {
	for _, imp := range pkg.Imports() {
		if stale[imp.Path()] {
			return true
		}
	}
	return false
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

// Importer implements types.ImporterFrom over two ExportSource tiers,
// decoding through gcexportdata and caching results in cache. Not safe for
// concurrent use across multiple Importer values sharing the same Cache
// without external synchronization beyond what Cache itself provides. A
// single Importer value, however, is designed for concurrent ImportFrom
// calls: only the map-mutating decode step holds Cache's lock, and sf
// collapses concurrent callers requesting the same path onto one decode.
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
// package's referenced types share identity with the rest of the build), so
// it cannot safely run concurrently with another decode against the same
// Cache. size is the raw export-data blob size (best effort; 0 if unknown),
// recorded in the cache's byte estimate for callers that bound cache growth
// by it (see Cache's own doc).
func (imp *Importer) decode(data []byte, path string, size int64) (*types.Package, error) {
	imp.cache.mu.Lock()
	defer imp.cache.mu.Unlock()
	pkg, err := gcexportdata.Read(bytes.NewReader(data), imp.fset, imp.cache.pkgs, path)
	if err != nil {
		return nil, fmt.Errorf("typecheck: decode export data for %s: %w", path, err)
	}
	imp.cache.bytes += size
	imp.cache.sizes[path] = size
	imp.cache.decodes++
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
