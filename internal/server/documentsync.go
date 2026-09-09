package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"

	bolterrors "go.etcd.io/bbolt/errors"
	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/index"
)

// handleDidOpen tracks the document's overlay content, focuses its
// package in the check engine (exempting it from LRU eviction), and
// schedules a recheck so diagnostics are published for a freshly opened
// file even before any edit. A file inside a graph-known NON-workspace
// package (GOROOT/module-cache — see workspace.nonWorkspacePackageForFile)
// skips ws.engine entirely: navigation for it is served by ws.depProvider
// instead (see resolveCheckedPackage), so focusing and scheduling an
// Engine recheck for it would only run its superseded export-data pipeline
// for no consumer — dependency source is immutable and assumed to compile
// (depcheck's own best-effort check.Config.Error), so it has no
// diagnostics worth publishing either.
//
// A didOpen arriving while s.workspace() is still nil — the async window
// between handleInitialize returning and its background graph load
// finishing (see lifecycle.go) — is queued via markPendingOpen instead of
// dropped: setWorkspace's own drainPendingOpens applies the same
// SetFocus/Invalidate to it once a workspace becomes available, mirroring
// handleDidSave's identical markDirty/drainDirty queue for a save landing
// while s.idx is nil — including its own re-check-after-mark race guard
// (see below): setWorkspace's Store/close/drainPendingOpens sequence can
// complete entirely in the window between this handler's own workspace()
// read and its markPendingOpen call, in which case that drain already ran
// over an empty pending set and would otherwise never see this path again.
func (s *Server) handleDidOpen(ctx context.Context, params json.RawMessage) error {
	var p protocol.DidOpenTextDocumentParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return err
	}
	s.overlay.DidOpen(&p)
	path := p.TextDocument.URI.FsPath()
	ws := s.workspace()
	if ws == nil {
		s.markPendingOpen(path)
		// Re-read s.workspace(): if setWorkspace's install-and-drain raced
		// this call and already finished, ws is non-nil here, and draining
		// ourselves (a no-op if the race did not actually happen — this
		// path is simply not in the pending set anymore) closes the gap
		// rather than leaving path queued with nothing left to drain it.
		if ws = s.workspace(); ws != nil {
			s.drainPendingOpens(ws)
		}
		return nil
	}
	if _, ok := ws.nonWorkspacePackageForFile(path); ok {
		return nil
	}
	ws.engine.SetFocus(path)
	ws.engine.Invalidate(filepath.Dir(path))
	s.selfHealFactsIfStale(ctx, ws, path)
	return nil
}

// selfHealFactsIfStale checks path's package against the facts index and
// schedules a background reindex if it disagrees with what is now on disk
// — closing the gap left by reindexing only ever being triggered by
// handleDidSave: a package changed outside the editor (git checkout, pull,
// branch switch) otherwise never gets its facts refreshed until one of its
// files happens to be saved through the editor, which may never happen for
// a file the user only ever reads. idx==nil is a no-op: the very first
// successful build writes every package's facts from scratch, so there is
// nothing to repair yet, and s.reindex would have no *indexState to write
// through regardless.
//
// The check itself (index.PackageChanged) is cheap enough to run inline —
// one bbolt read plus stat-based hashing of path's own package files, no
// type-checking — but the reindex it can trigger is not, so that part is
// detached via s.rpc.Go exactly like handleDidSave's own reindex, rather
// than blocking this notification handler's return.
func (s *Server) selfHealFactsIfStale(ctx context.Context, ws *workspace, path string) {
	idx := s.idx.Load()
	if idx == nil {
		return
	}
	pkgPath, ok := s.pkgPathForFile(path)
	if !ok {
		return
	}
	changed, err := index.PackageChanged(ctx, ws.snap, idx.db, pkgPath, runtime.Version(), "", RelativeIndexPaths(ws.root))
	if err != nil {
		s.logger.Printf("golance: check facts for %s: %v", pkgPath, err)
		return
	}
	if !changed {
		return
	}
	s.rpc.Go(func(ctx context.Context) { s.reindexIfStillCurrent(ctx, ws, idx, pkgPath) })
}

// handleDidChange applies the content change to the document's overlay and
// schedules a debounced recheck of its package. See handleDidOpen's doc for
// why a graph-known non-workspace file skips ws.engine.
func (s *Server) handleDidChange(_ context.Context, params json.RawMessage) error {
	var p protocol.DidChangeTextDocumentParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return err
	}
	if err := s.overlay.DidChange(&p); err != nil {
		return err
	}
	ws := s.workspace()
	if ws == nil {
		return nil
	}
	path := p.TextDocument.URI.FsPath()
	if _, ok := ws.nonWorkspacePackageForFile(path); ok {
		return nil
	}
	ws.engine.Invalidate(filepath.Dir(path))
	return nil
}

// handleDidSave refreshes the document's overlay with the saved text (if
// the client included it), schedules a recheck, and reindexes the saved
// package in the background — immediately if the facts index is ready, or
// once it becomes ready otherwise (see markDirty/drainDirty), rather than
// simply dropping the save: the facts index is nil not just before the
// very first successful build, but also briefly whenever revalidateIndex
// swaps out a stale one for a rebuilt one.
func (s *Server) handleDidSave(_ context.Context, params json.RawMessage) error {
	var p protocol.DidSaveTextDocumentParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return err
	}
	if err := s.overlay.DidSave(&p); err != nil {
		return err
	}
	ws := s.workspace()
	if ws == nil {
		return nil
	}
	path := p.TextDocument.URI.FsPath()
	// A save inside a graph-known non-workspace package (GOROOT/module-cache
	// — see handleDidOpen's doc) needs neither an Engine recheck nor
	// reindexing: dependency source outside the workspace is never a facts
	// index root (internal/index/scheduler.go's doc), and navigation for it
	// is served by ws.depProvider, not ws.engine.
	if _, ok := ws.nonWorkspacePackageForFile(path); ok {
		return nil
	}
	ws.engine.Invalidate(filepath.Dir(path))

	pkgPath, ok := s.pkgPathForFile(path)
	if !ok {
		return nil
	}
	idx := s.idx.Load()
	if idx == nil {
		s.markDirty(pkgPath)
		// Re-read s.idx: openIndexAfterBuild may have installed it and
		// already drained the dirty set in the window between our own Load
		// above and markDirty just now, in which case this save's pkgPath
		// would otherwise sit unindexed until some unrelated later change
		// happens to drain it again. If that race did happen, idx is
		// non-nil here, and draining ourselves closes the gap; if it
		// didn't, this is simply nil again and openIndexAfterBuild's own
		// eventual drainDirty call picks pkgPath up as usual. Either way
		// this is at most a harmless duplicate reindex, never a lost one.
		if idx = s.idx.Load(); idx != nil {
			s.rpc.Go(func(ctx context.Context) { s.drainDirty(ctx, ws) })
		}
		return nil
	}
	// This reindex is detached from the notification that triggered it (it
	// can run well past handleDidSave's own return), so it must not use a
	// per-notification ctx or an unbounded context.Background(): s.rpc.Go
	// binds it to the session's own lifetime instead — canceled once Serve
	// returns — and tracks it via Serve's own wg, so shutdown waits
	// (briefly) for it to finish or notice cancellation, rather than
	// abandoning it mid-write.
	s.rpc.Go(func(ctx context.Context) { s.reindexIfStillCurrent(ctx, ws, idx, pkgPath) })
	return nil
}

// markDirty records pkgPath as saved while no facts index was available
// (see handleDidSave), pending reindex once one becomes available (see
// drainDirty).
func (s *Server) markDirty(pkgPath string) {
	s.dirtyMu.Lock()
	defer s.dirtyMu.Unlock()
	if s.dirtyPkgs == nil {
		s.dirtyPkgs = make(map[string]bool)
	}
	s.dirtyPkgs[pkgPath] = true
}

// takeDirty returns every package path recorded via markDirty since the
// last takeDirty call, clearing the set.
func (s *Server) takeDirty() []string {
	s.dirtyMu.Lock()
	defer s.dirtyMu.Unlock()
	if len(s.dirtyPkgs) == 0 {
		return nil
	}
	pkgs := make([]string, 0, len(s.dirtyPkgs))
	for p := range s.dirtyPkgs {
		pkgs = append(pkgs, p)
	}
	s.dirtyPkgs = nil
	return pkgs
}

// drainDirty reindexes every package markDirty recorded while the facts
// index was unavailable, if it is available now — a no-op if s.idx is
// still nil, or if nothing is dirty. Called from two places that can each
// observe s.idx transition from nil to installed: openIndexAfterBuild,
// right after installing a freshly built index, and handleDidSave itself,
// when a save's own idx.Load() raced that installation (see its doc). Both
// ultimately drain the same underlying set, so a call from both in that
// race window just reindexes the same package(s) twice — extra work, never
// a lost save.
func (s *Server) drainDirty(ctx context.Context, ws *workspace) {
	idx := s.idx.Load()
	if idx == nil {
		return
	}
	for _, pkgPath := range s.takeDirty() {
		s.reindexIfStillCurrent(ctx, ws, idx, pkgPath)
	}
}

// handleDidClose stops tracking the document's overlay content; its
// package falls back to on-disk content on the next check.
func (s *Server) handleDidClose(_ context.Context, params json.RawMessage) error {
	var p protocol.DidCloseTextDocumentParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return err
	}
	s.overlay.DidClose(&p)
	return nil
}

// beginReindex registers a detached reindex attempt against idx, reporting
// ok=false (nothing registered) if idx's own database is no longer the one
// s.idx currently points at. Guarded end-to-end by s.idxMu — the same lock
// revalidateIndex's rebuild branch (indexer.go) holds across its own
// Store(nil)/reindexWG.Wait()/Close() sequence — so the two can never
// interleave: either this call's check-and-register happens entirely before
// that rebuild's Store(nil) (in which case its later Wait() correctly
// blocks on the registration this call just made, and Close() cannot run
// until the matching Done() below), or entirely after it (in which case
// s.idx.Load() already disagrees with idx, and this correctly reports
// ok=false instead of registering against a database about to be closed).
// There is no window in between where a registration could land after
// Wait() has already decided to return.
//
// The comparison is idx.db against s.idx.Load()'s own db, not the whole
// *indexState pointer: setWorkspace also replaces s.idx with a fresh
// *indexState wrapping the SAME db (only its resolver refreshed against a
// new graph snapshot) on every ordinary workspace reload, which must not be
// mistaken for the database itself having been closed.
func (s *Server) beginReindex(idx *indexState) bool {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	cur := s.idx.Load()
	if cur == nil || cur.db != idx.db {
		return false
	}
	s.reindexWG.Add(1)
	return true
}

// reindexIfStillCurrent runs reindex against idx only if beginReindex
// confirms idx's own database is still current, bailing out otherwise
// without ever attempting a write against it. Both selfHealFactsIfStale and
// handleDidSave dispatch their background reindex through this rather than
// calling reindex directly: idx is captured well before this actually runs
// (s.rpc.Go's own goroutine-scheduling delay, plus reindex's own
// type-checking time), during which revalidateIndex's rebuild branch can
// close idx.db out from under a dispatch that raced it — neither
// selfHealFactsIfStale nor handleDidSave holds s.idxMu while dispatching,
// deliberately, so a save or open is never blocked behind a concurrent
// rebuild; only this call's own brief registration step does.
//
// Bailing out here can never silently drop a repair still needed:
// index.PackageChanged (selfHealFactsIfStale) and index.RevalidateStale
// (revalidateIndex) both re-derive pkgPath's staleness from its on-disk
// content against whatever db ends up current, independent of whether this
// particular write ran — the very rebuild that closed idx.db already
// re-type-checks pkgPath fresh, and a revalidateIndex pass against whatever
// index is open next catches it otherwise. A bailed-out reindex is
// therefore redundant work skipped, never a permanently lost fix.
func (s *Server) reindexIfStillCurrent(ctx context.Context, ws *workspace, idx *indexState, pkgPath string) {
	if !s.beginReindex(idx) {
		return
	}
	defer s.reindexWG.Done()
	s.reindex(ctx, ws, idx, pkgPath)
}

// reindexDBClosedUnderfoot reports whether err — a failure from
// index.Reindex's own persist step — means idx.db was closed while this
// call was still running, rather than any other write failure. reindexWG
// (see beginReindex) makes this unreachable for the one Close call site
// this package fully controls (revalidateIndex's rebuild branch, indexer.go),
// but idx.db is an ordinary *store.DB a caller outside this package's
// control could also close directly — a test harness that owns the same db
// handle newTestServer wired into s.idx being the concrete case this guards
// against, closing it via t.Cleanup with no knowledge of a self-heal
// dispatch racing it. reindex treats this outcome exactly like reindexIfStillCurrent's
// own bail-out: silently redundant, never a lost repair (see its doc).
func reindexDBClosedUnderfoot(err error) bool {
	return errors.Is(err, bolterrors.ErrDatabaseNotOpen)
}

// reindex re-type-checks pkgPath (and, if its export data changed, its
// reverse-dependency closure) and persists the result to idx.db. On
// success, it also drops pkgPath and every reverse-dependency-closure hop
// Reindex actually reprocessed (Stats.Changed) from the check engine's
// persistent dependency cache, from ws.engine's own per-unit cache, and from
// idx.resolver's own export-data cache, so a later recheck of an open
// file — or a later cross-reference query, e.g. Go to Implementation —
// re-decodes freshly written export data instead of reusing a *types.Package
// decoded from what was on disk before this save (see xref.Resolver.Invalidate's
// doc for why that reuse would otherwise happen silently, and
// check.Engine.InvalidateDependency's doc for why ws.engine's own cache
// needs the identical treatment: its content-hash cache hit test only ever
// looks at a unit's OWN files, so a dependency's export data changing
// underneath it leaves a hit indefinitely stale otherwise). Narrowing to
// Stats.Changed instead of the whole closure Reindex walked is sound: a hop
// Reindex skipped had a combined blob key that provably matched what db
// already had, so its export data provably did not change either — the same
// guarantee unchangedOutcome already relies on inside Reindex itself. If
// Stats.Changed comes back empty (Reindex found pkgPath itself
// byte-identical too), fall back to invalidating pkgPath alone: the overlay
// content this save just wrote may still differ from what was on disk when
// Reindex's own trustStat check ran.
func (s *Server) reindex(ctx context.Context, ws *workspace, idx *indexState, pkgPath string) {
	stats, err := index.Reindex(ctx, ws.snap, idx.db, idx.cas, pkgPath, s.overlay.ReadFile, &index.Options{RelativePaths: RelativeIndexPaths(ws.root)})
	if err != nil {
		if reindexDBClosedUnderfoot(err) {
			return
		}
		s.logger.Printf("server: reindex %s: %v", pkgPath, err)
		return
	}
	changed := stats.Changed
	if len(changed) == 0 {
		changed = []string{pkgPath}
	}
	ws.depCache.invalidate(changed)
	ws.engine.InvalidateDependency(closureDirs(ws, changed))
	idx.resolver.Invalidate(changed)
}

// closureDirs resolves each of pkgPaths (Stats.Changed, see reindex) to its
// package directory via ws.snap, for ws.engine.InvalidateDependency — Engine
// itself keys its cache by directory, not import path. A pkgPath ws.snap no
// longer recognizes (removed from the workspace since Reindex last read it,
// vanishingly unlikely within one reindex call but not provably impossible)
// is silently skipped: ws.engine can hold nothing cached under a directory
// it was never told about in the first place.
func closureDirs(ws *workspace, pkgPaths []string) []string {
	dirs := make([]string, 0, len(pkgPaths))
	for _, pkgPath := range pkgPaths {
		if pkg, ok := ws.snap.Package(pkgPath); ok {
			dirs = append(dirs, pkg.Dir)
		}
	}
	return dirs
}
