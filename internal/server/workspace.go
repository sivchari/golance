package server

import (
	"context"
	"encoding/json"
	"go/token"
	"go/types"
	"path/filepath"
	"sync"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/typecheck"
	"github.com/sivchari/golance/internal/xref"
)

// maxDepCacheBytes bounds depCache's naive byte estimate (the sum of
// decoded export-data blob sizes, see typecheck.Cache.Bytes) before it is
// discarded and replaced with an empty one. v0.1 has no precise heap
// accounting for decoded *types.Package values, so this is deliberately a
// coarse, whole-cache eviction rather than a per-package LRU.
const maxDepCacheBytes = 512 * 1024 * 1024 // 512MiB

// depCacheHolder owns the persistent typecheck.Cache the check engine's
// dependency importer decodes into across many rechecks, plus the single
// *token.FileSet it is tied to (see typecheck.Cache's doc: a Cache and the
// fset it was decoded into must be discarded together, never
// independently). files resolves external-module and stdlib export data
// (typecheck.ExportFileSource); those are treated as immutable for the
// life of a workspace, since any change to them implies a go.mod/go.sum
// change, which already triggers a full setWorkspace (and so a fresh
// depCacheHolder) via revalidateGraph.
type depCacheHolder struct {
	files typecheck.ExportFileSource

	mu    sync.Mutex
	fset  *token.FileSet
	cache *typecheck.Cache
}

func newDepCacheHolder(files typecheck.ExportFileSource) *depCacheHolder {
	return &depCacheHolder{files: files, fset: token.NewFileSet(), cache: typecheck.NewCache()}
}

// importer returns a types.ImporterFrom decoding into d's current
// (fset, cache) pair, first swapping in a fresh, empty pair if the current
// one has grown past maxDepCacheBytes.
func (d *depCacheHolder) importer() types.ImporterFrom {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache.Bytes() > maxDepCacheBytes {
		d.fset = token.NewFileSet()
		d.cache = typecheck.NewCache()
	}
	return typecheck.NewImporter(d.fset, nil, d.files, d.cache)
}

// invalidate drops pkgPaths from the current cache, so the next recheck
// that imports any of them re-decodes fresh export data instead of reusing
// a now-possibly-stale *types.Package. Callers use this after a workspace
// package's on-disk export data changes (didSave's background reindex).
func (d *depCacheHolder) invalidate(pkgPaths []string) {
	d.mu.Lock()
	cache := d.cache
	d.mu.Unlock()
	for _, p := range pkgPaths {
		cache.Delete(p)
	}
}

// setWorkspace builds a fresh workspace bundle over snap and installs it,
// replacing whatever workspace (if any) was loaded before. If a facts
// index is already open, its Resolver is rebuilt over the new snapshot too
// (the *store.DB itself is untouched — only the in-memory import-graph
// view a Resolver holds needs refreshing).
func (s *Server) setWorkspace(root string, snap *graph.Snapshot) {
	src := check.NewGraphSource(snap)
	depCache := newDepCacheHolder(snap)
	imp := depCache.importer
	engine := check.New(src, s.overlay, imp, check.Options{OnResult: s.publishDiagnostics})

	fileToPkg := make(map[string]string)
	for pkgPath, pkg := range snap.Packages {
		for _, f := range pkg.GoFiles {
			fileToPkg[f] = pkgPath
		}
	}

	s.ws.Store(&workspace{root: root, snap: snap, engine: engine, fileToPkg: fileToPkg, depCache: depCache})

	if idx := s.idx.Load(); idx != nil {
		s.idx.Store(&indexState{db: idx.db, cas: idx.cas, resolver: xref.New(idx.db, idx.cas, snap, RelativeIndexPaths(root))})
	}
}

// revalidateGraph reloads the import graph from scratch and installs it as
// the current workspace, refreshing the on-disk cache. Used both for a
// stale-cache background revalidation right after initialize and for a
// workspace/didChangeWatchedFiles-triggered reload.
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
}

// handleDidChangeWatchedFiles reloads the import graph in the background
// when go.mod, go.sum, go.work, or go.work.sum changes, since any of those
// can change what packages.Load would compute.
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
			go s.revalidateGraph(graph.Options{Dir: ws.root, Offline: s.opts.Offline}, []string{"./..."})
			return nil
		}
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
