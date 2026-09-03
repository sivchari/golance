package server

import (
	"context"
	"encoding/json"
	"path/filepath"

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
func (s *Server) handleDidOpen(_ context.Context, params json.RawMessage) error {
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
	return nil
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
	s.rpc.Go(func(ctx context.Context) { s.reindex(ctx, ws, idx, pkgPath) })
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
		s.reindex(ctx, ws, idx, pkgPath)
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

// reindex re-type-checks pkgPath (and, if its export data changed, its
// reverse-dependency closure) and persists the result to idx.db. On
// success, it also drops pkgPath and every package that (transitively)
// imports it from the check engine's persistent dependency cache and from
// idx.resolver's own export-data cache, so a later recheck of an open file
// — or a later cross-reference query, e.g. Go to Implementation — re-decodes
// pkgPath's freshly written export data instead of reusing the
// *types.Package decoded from what was on disk before this save (see
// xref.Resolver.Invalidate's doc for why that reuse would otherwise happen
// silently). This is deliberately coarser than Reindex's own change-
// propagation (which stops at the first hop whose export data didn't
// actually change): it is sound either way, and avoids needing Reindex to
// report exactly which hops in the closure changed.
func (s *Server) reindex(ctx context.Context, ws *workspace, idx *indexState, pkgPath string) {
	if _, err := index.Reindex(ctx, ws.snap, idx.db, idx.cas, pkgPath, s.overlay.ReadFile, &index.Options{RelativePaths: RelativeIndexPaths(ws.root)}); err != nil {
		s.logger.Printf("server: reindex %s: %v", pkgPath, err)
		return
	}
	changed := append([]string{pkgPath}, ws.snap.ClosureUnits(pkgPath)...)
	ws.depCache.invalidate(changed)
	idx.resolver.Invalidate(changed)
}
