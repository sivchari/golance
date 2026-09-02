package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/typecheck"
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

// ensureDepProvider returns the depcheck.Provider setWorkspace should
// install into the workspace it is building over snap: the server's current
// one, retargeted at snap, if snap's dependency set (depsKey) matches the
// one the current Provider was built for — otherwise a fresh Provider (and
// a fresh depMetadataSource) replacing it.
//
// This is the fix for a real production stall (see the caller's own doc):
// setWorkspace runs on every graph revalidation, not only at initialize —
// any go.mod/go.sum/go.work change, and any workspace/didChangeWatchedFiles
// batch that adds or removes a file in an already-known package directory
// (see needsGraphReload) — which, before this, discarded depProvider's
// entire type-check cache every time. Since depProvider exists specifically
// to answer navigation into the standard library and module dependencies —
// content that does not change just because the user edited a workspace
// file — that discard bought nothing but forced the next dependency-facing
// query to re-type-check its whole import closure cold, sometimes tens of
// seconds' worth of work for a large monorepo's shared dependencies.
// Reusing the Provider whenever the dependency set itself is unchanged
// removes that cost entirely for the common case (an ordinary edit inside
// the workspace), while still rebuilding it — correctly, from scratch —
// whenever the dependency set genuinely could have changed.
func (s *Server) ensureDepProvider(snap *graph.Snapshot) *depcheck.Provider {
	key := depsKey(snap)

	s.depProviderMu.Lock()
	defer s.depProviderMu.Unlock()
	if s.depProviderVal == nil || key != s.depProviderKey {
		s.depProviderSrc = &depMetadataSource{}
		s.depProviderVal = depcheck.NewProvider(s.depProviderSrc, depcheck.Options{})
		s.depProviderKey = key
	}
	s.depProviderSrc.retarget(snap)
	return s.depProviderVal
}

// setWorkspace builds a fresh workspace bundle over snap and installs it,
// replacing whatever workspace (if any) was loaded before. If a facts
// index is already open, its Resolver is rebuilt over the new snapshot too
// (the *store.DB itself is untouched — only the in-memory import-graph
// view a Resolver holds needs refreshing).
func (s *Server) setWorkspace(root string, snap *graph.Snapshot) {
	src := check.NewGraphSource(snap, s.overlay)
	depCache := newDepCacheHolder(snap)
	imp := depCache.importer
	engine := check.New(src, s.overlay, imp, check.Options{OnResult: s.publishDiagnostics})
	depProvider := s.ensureDepProvider(snap)

	fileToPkg := make(map[string]string)
	dirToPkg := make(map[string]string, len(snap.Packages))
	for pkgPath, pkg := range snap.Packages {
		// Skip a ForTest-tagged entry: a synthesized test-only node (most
		// commonly an external "_test" package) that can share pkg.Dir
		// with the ordinary package it tests under a different PkgPath —
		// see internal/check.GraphSource's identical exclusion for why
		// indexing it here too would risk misrouting a new file in that
		// directory to an unimportable PkgPath.
		if pkg.ForTest != "" {
			continue
		}
		for _, f := range pkg.GoFiles {
			fileToPkg[f] = pkgPath
		}
		dirToPkg[pkg.Dir] = pkgPath
	}
	pkgNameIndex := buildPkgNameIndex(snap)

	// Stop the outgoing workspace's engine before installing the new one:
	// otherwise a debounce timer already scheduled on it (e.g. by a
	// handleDidChange that captured the old workspace microseconds before
	// this swap) could still fire afterward and publish diagnostics
	// computed against the now-discarded import graph.
	if old := s.ws.Load(); old != nil {
		old.engine.Stop()
	}
	s.ws.Store(&workspace{
		root: root, snap: snap, engine: engine, fileToPkg: fileToPkg, dirToPkg: dirToPkg,
		depCache: depCache, depProvider: depProvider, pkgNameIndex: pkgNameIndex,
	})

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
			go s.revalidateGraph(graph.Options{Dir: ws.root, Offline: s.opts.Offline}, []string{allPackagesPattern})
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
// all, deferred to the next restart (see needsGraphReload's doc). Either
// way, the facts index is then revalidated exactly like the once-at-startup
// check (see revalidateIndex): if it disagrees with what is now on disk,
// the indexer subprocess rebuilds it in the background exactly as it does
// on a cold start.
func (s *Server) revalidateWorkspace(root string, reload bool) {
	if reload {
		s.revalidateGraph(graph.Options{Dir: root, Offline: s.opts.Offline}, []string{allPackagesPattern})
	}
	// s.watch (see watch.go) calls this from its own debounce-timer
	// goroutine, not from a request/notification handler, so there is no
	// handler-scoped ctx to thread through here: s.rpc.Context() is the
	// session-lifetime context that binds the indexer subprocess this may
	// launch to the server's own shutdown (see revalidateIndex).
	s.revalidateIndex(s.rpc.Context(), root)
}
