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
// file even before any edit.
func (s *Server) handleDidOpen(_ context.Context, params json.RawMessage) error {
	var p protocol.DidOpenTextDocumentParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return err
	}
	s.overlay.DidOpen(&p)
	ws := s.workspace()
	if ws == nil {
		return nil
	}
	path := p.TextDocument.URI.FsPath()
	ws.engine.SetFocus(path)
	ws.engine.Invalidate(filepath.Dir(path))
	return nil
}

// handleDidChange applies the content change to the document's overlay and
// schedules a debounced recheck of its package.
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
	ws.engine.Invalidate(filepath.Dir(p.TextDocument.URI.FsPath()))
	return nil
}

// handleDidSave refreshes the document's overlay with the saved text (if
// the client included it), schedules a recheck, and — if the facts index
// is ready — reindexes the saved package in the background.
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
	ws.engine.Invalidate(filepath.Dir(path))

	pkgPath, ok := s.pkgPathForFile(path)
	idx := s.idx.Load()
	if !ok || idx == nil {
		return nil
	}
	go s.reindex(ws, idx, pkgPath)
	return nil
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
// imports it from the check engine's persistent dependency cache, so a
// later recheck of an open file in one of those packages re-decodes
// pkgPath's freshly written export data instead of reusing the
// *types.Package decoded from what was on disk before this save. This is
// deliberately coarser than Reindex's own change-propagation (which stops
// at the first hop whose export data didn't actually change): it is sound
// either way, and avoids needing Reindex to report exactly which hops in
// the closure changed.
func (s *Server) reindex(ws *workspace, idx *indexState, pkgPath string) {
	if _, err := index.Reindex(context.Background(), ws.snap, idx.db, idx.cas, pkgPath, s.overlay.ReadFile, index.Options{RelativePaths: RelativeIndexPaths(ws.root)}); err != nil {
		s.logger.Printf("server: reindex %s: %v", pkgPath, err)
		return
	}
	ws.depCache.invalidate(append([]string{pkgPath}, ws.snap.ClosureUnits(pkgPath)...))
}
