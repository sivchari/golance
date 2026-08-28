// Package typecheck runs go/types over a single package's already-parsed
// files, resolving its dependencies from export data rather than
// re-type-checking them. Two sources are tried in order: a caller-supplied
// ExportSource (self-authored blobs, e.g. from a prior WriteExport of a
// workspace package) and an ExportFileSource (the GOCACHE-generated export
// files go/packages reports for stdlib and module dependencies).
package typecheck

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sync/singleflight"
	"golang.org/x/tools/go/gcexportdata"
)

// ExportSource resolves self-authored export data (written by WriteExport)
// for a workspace package, keyed by import path. ok is false when the
// source has no data for pkgPath — the importer then falls back to an
// ExportFileSource.
type ExportSource interface {
	ExportData(pkgPath string) (data []byte, ok bool, err error)
}

// ExportFileSource resolves the GOCACHE-generated export data file for a
// package, as reported by go/packages' NeedExportFile mode. graph.Snapshot
// satisfies this interface.
type ExportFileSource interface {
	ExportFile(pkgPath string) (file string, ok bool)
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
	bytes   int64 // sum of decoded export-data blob sizes, a naive proxy for memory held
	decodes int64 // number of gcexportdata.Read calls this Cache has performed (cache misses)
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{pkgs: make(map[string]*types.Package)}
}

// Delete removes pkgPath's cached *types.Package, if any. A later
// ImportFrom(pkgPath, ...) re-decodes it from export data. Callers use this
// to evict dependencies once every importer that needed them has finished,
// bounding cache growth independent of workspace size.
func (c *Cache) Delete(pkgPath string) {
	c.mu.Lock()
	delete(c.pkgs, pkgPath)
	c.mu.Unlock()
}

// Len returns the number of *types.Package values currently cached.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pkgs)
}

// Bytes returns the running total of decoded export-data blob sizes: a
// cheap, approximate estimate of the memory c is holding onto, for callers
// that want to bound cache growth (e.g. discard c and start a fresh one
// past some threshold) without a precise heap accounting.
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

// Importer implements types.ImporterFrom over an ExportSource and an
// ExportFileSource, decoding through gcexportdata and caching results in
// cache. Not safe for concurrent use across multiple Importer values
// sharing the same Cache without external synchronization beyond what
// Cache itself provides. A single Importer value, however, is designed for
// concurrent ImportFrom calls (see its doc): only the map-mutating decode
// step holds Cache's lock, and sf collapses concurrent callers requesting
// the same path onto one decode.
type Importer struct {
	fset  *token.FileSet
	src   ExportSource
	files ExportFileSource
	cache *Cache
	sf    singleflight.Group
}

// NewImporter returns an Importer that resolves imports via src first, then
// files, decoding into fset and caching results in cache. Either src or
// files may be nil to skip that source.
func NewImporter(fset *token.FileSet, src ExportSource, files ExportFileSource, cache *Cache) *Importer {
	return &Importer{fset: fset, src: src, files: files, cache: cache}
}

// Import implements types.Importer.
func (imp *Importer) Import(path string) (*types.Package, error) {
	return imp.ImportFrom(path, "", 0)
}

// ImportFrom implements types.ImporterFrom. dir and mode are accepted for
// interface compliance but unused: export data resolution here is keyed
// purely by import path.
//
// Concurrent ImportFrom calls (from concurrent CheckPackage runs sharing
// this Importer) only serialize on Cache's lock for the brief map lookup
// and, per distinct path, the gcexportdata.Read call that mutates the
// shared imports map. Resolving the export data itself — an ExportSource
// lookup, or opening and reading an ExportFileSource file — runs outside
// that lock, and singleflight collapses concurrent callers for the same
// uncached path onto a single resolve instead of each repeating the I/O.
func (imp *Importer) ImportFrom(path, _ string, _ types.ImportMode) (*types.Package, error) {
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
	return v.(*types.Package), nil
}

// cacheGet returns path's cached, fully-decoded *types.Package, if any.
func (imp *Importer) cacheGet(path string) (*types.Package, bool) {
	imp.cache.mu.Lock()
	defer imp.cache.mu.Unlock()
	pkg, ok := imp.cache.pkgs[path]
	return pkg, ok && pkg.Complete()
}

// resolve locates path's export data — via imp.src first, then imp.files —
// and decodes it. Locating and reading the data (an ExportSource lookup, or
// opening and reading an ExportFileSource file) does not touch imp.cache
// and so needs no lock; only decode does.
func (imp *Importer) resolve(path string) (*types.Package, error) {
	if imp.src != nil {
		data, ok, err := imp.src.ExportData(path)
		if err != nil {
			return nil, fmt.Errorf("typecheck: read export data for %s: %w", path, err)
		}
		if ok {
			return imp.decode(bytes.NewReader(data), path, int64(len(data)))
		}
	}

	if imp.files == nil {
		return nil, fmt.Errorf("typecheck: no export data for %s", path)
	}
	file, ok := imp.files.ExportFile(path)
	if !ok {
		return nil, fmt.Errorf("typecheck: no export data for %s", path)
	}
	f, err := os.Open(filepath.Clean(file))
	if err != nil {
		return nil, fmt.Errorf("typecheck: open export file for %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	r, err := gcexportdata.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("typecheck: read export header for %s: %w", path, err)
	}
	return imp.decode(r, path, size)
}

// decode runs gcexportdata.Read under Cache's lock. The call both reads and
// mutates imp.cache.pkgs (the shared imports map, required so a decoded
// package's referenced types share identity with the rest of the build),
// so it cannot safely run concurrently with another decode against the
// same Cache. size is the raw export-data blob size (best effort; 0 if
// unknown), recorded in the cache's byte estimate for callers that bound
// cache growth by it.
func (imp *Importer) decode(r io.Reader, path string, size int64) (*types.Package, error) {
	imp.cache.mu.Lock()
	defer imp.cache.mu.Unlock()
	pkg, err := gcexportdata.Read(r, imp.fset, imp.cache.pkgs, path)
	if err != nil {
		return nil, fmt.Errorf("typecheck: decode export data for %s: %w", path, err)
	}
	imp.cache.bytes += size
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
