package server

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"math"
	"os"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/xref"
)

// resolverOrWarn returns the current facts-index Resolver, or ok=false if
// the indexer subprocess has not completed a build yet. On the first such
// call it logs the reason to the client once via window/logMessage (not
// showMessage: the index building is routine, not a failure, and some
// clients render showMessage as a blocking modal — see logMessage's doc);
// later calls stay silent so a burst of queries during index build does not
// spam the log. Callers still answer with an ordinary empty result (see
// handleDefinition et al.) rather than an error, since an empty result is
// exactly how a client already renders "nothing found" — $/progress (see
// relayIndexProgress) is what actually tells the user a build is under way.
func (s *Server) resolverOrWarn() (*xref.Resolver, bool) {
	idx := s.idx.Load()
	if idx == nil {
		if s.indexBuildingWarned.CompareAndSwap(false, true) {
			s.logMessage(protocol.MessageTypeInfo, "golance: the workspace index is still building; cross-reference results are unavailable until it completes")
		}
		return nil, false
	}
	return idx.resolver, true
}

// xrefPosition converts an LSP Position for path's current editor buffer
// into the 1-based line/byte-column coordinates internal/xref queries
// take, correcting for any unsaved edits (see dirty.go) since xref answers
// from the on-disk facts index.
func (s *Server) xrefPosition(path string, pos protocol.Position) (line, col int, ok bool) {
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	l, c, ok := positionToXref(text, pos)
	if !ok {
		return 0, 0, false
	}
	if l < 0 {
		l = 0
	}
	if l > math.MaxUint32 {
		l = math.MaxUint32
	}
	return int(s.correctQueryLine(path, uint32(l))), c, true
}

// toLSPLocations converts xref Locations to LSP Locations, applying dirty
// correction per result file and dropping any that cannot be resolved.
func (s *Server) toLSPLocations(locs []xref.Location) protocol.LocationSlice {
	out := make(protocol.LocationSlice, 0, len(locs))
	for _, loc := range locs {
		if pl, ok := s.correctResultLocation(loc); ok {
			out = append(out, pl)
		}
	}
	return out
}

func (s *Server) correctResultLocation(loc xref.Location) (protocol.Location, bool) {
	rng, ok := s.correctResultRange(loc.File, loc.Line, loc.Col, loc.EndCol)
	if !ok {
		return protocol.Location{}, false
	}
	return protocol.Location{URI: uri.File(loc.File), Range: rng}, true
}

func (s *Server) handleDefinition(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.DefinitionParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return s.definitionFallback(ctx, p.TextDocument.URI, p.Position), nil
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	locs, err := resolver.Definition(ctx, path, line, col)
	if err != nil {
		// Most errors here mean "no symbol at this position" — a routine
		// outcome the LSP client already handles as an empty result, not
		// something to surface as a protocol error. Log it so a genuine
		// facts-read failure is still visible, rather than silently
		// indistinguishable from an ordinary miss.
		s.logger.Printf("server: definition at %s:%d:%d: %v", path, line, col, err)
		return s.definitionFallback(ctx, p.TextDocument.URI, p.Position), nil
	}
	if len(locs) == 0 {
		// The facts index only ever covers root (workspace) packages (see
		// internal/index/scheduler.go's doc) and can otherwise legitimately
		// have no entry for a resolvable position; fall through to the
		// same type-info-based path used when the index cannot be
		// consulted at all, rather than treating an empty index answer as
		// final.
		return s.definitionFallback(ctx, p.TextDocument.URI, p.Position), nil
	}
	return s.toLSPLocations(locs), nil
}

// definitionFallback answers handleDefinition entirely from the
// type-checked package's own AST/types.Info/FileSet, for whenever the
// workspace facts index cannot answer: it has not finished building yet
// (resolverOrWarn's ok=false), a store query against it failed, or it
// legitimately has no entry for this position. An identifier declared in
// cp's own package resolves via langfeat.SamePackageDefinition, exact down
// to the column, needing no index at all; a standard library or module
// dependency identifier resolves through dependencyDefinition's export-data
// path instead, degraded to column 1. A different *workspace* (root)
// package's identifier is deliberately left unanswered here — see
// dependencyDefinition's doc for why.
func (s *Server) definitionFallback(ctx context.Context, u uri.URI, pos protocol.Position) protocol.LocationSlice {
	cf := s.checkedFile(ctx, u, pos)
	if !cf.ok {
		return nil
	}
	if loc, ok := s.importDefinition(cf); ok {
		return s.toLSPLocations([]xref.Location{loc})
	}
	info, err := langfeat.SamePackageDefinition(cf.cp, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: same-package definition %s: %v", cf.path, err)
	} else if info != nil {
		return s.samePackageDefinitionLocation(info)
	}
	if loc, ok := s.dependencyDefinition(cf); ok {
		return s.toLSPLocations([]xref.Location{loc})
	}
	return nil
}

// samePackageDefinitionLocation converts a SamePackageDefInfo (byte offsets
// against info.File's own current buffer) into an LSP location, the same
// pattern typeDefinitionSameFile uses for langfeat.TypeDefInfo.
func (s *Server) samePackageDefinitionLocation(info *langfeat.SamePackageDefInfo) protocol.LocationSlice {
	text, err := s.overlay.ReadFile(info.File)
	if err != nil {
		s.logger.Printf("server: same-package definition read %s: %v", info.File, err)
		return nil
	}
	rng, ok := offsetRangeToLSP(text, info.Range.StartOffset, info.Range.EndOffset)
	if !ok {
		return nil
	}
	return protocol.LocationSlice{{URI: uri.File(info.File), Range: rng}}
}

// dependencyDefinition is definitionFallback's path for a symbol the
// workspace facts index has no answer for and that is not declared in cf's
// own package: the facts index only ever covers root (workspace) packages
// (see internal/index/scheduler.go's doc), so a definition query on an
// identifier from the standard library or a module dependency always
// misses there. This resolves it instead through the type-checked
// package's own Uses/Defs and the shared dependency importer's export-data
// positions (see internal/langfeat.DependencyDefinition,
// depCacheHolder.FileSet) — the same decode already paid for to type-check
// cf.path in the first place, not a separate source parse of the
// dependency.
func (s *Server) dependencyDefinition(cf checkedFileResult) (xref.Location, bool) {
	ws := s.workspace()
	if ws == nil {
		return xref.Location{}, false
	}
	info, err := langfeat.DependencyDefinition(cf.cp, ws.depCache.FileSet(), cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: dependency definition %s: %v", cf.path, err)
		return xref.Location{}, false
	}
	if info == nil {
		return xref.Location{}, false
	}
	// A root (workspace) package's export data is never offered as a
	// substitute for the facts index's own answer, even from
	// definitionFallback (the index-unavailable path): TestE2E_WorktreeSharesIndex
	// pinned a real hazard this used to create. A second session (or a
	// cold-start session, before its own index build finishes) treats a
	// non-empty textDocument/definition result as a signal that the index
	// is now usable — e.g. the E2E suite's waitForNonEmptyLocations, and a
	// real editor racing a request right after the index-build progress
	// notification fires — and, per handleDidSave, an edit saved while
	// s.idx is still nil never gets incrementally reindexed at all (no
	// retry once the index later opens). Answering a workspace package's
	// position via export data here would let that race succeed on stale
	// grounds — the index build might complete moments later with the
	// authoritative, exact-column answer — silently dropping the
	// reindex-after-save the caller would otherwise still be waiting for.
	// The standard library and module dependencies carry no such risk
	// (nothing in this workspace ever reindexes them), so only this case is
	// excluded.
	if pkg, ok := ws.snap.Packages[info.PkgPath]; ok && pkg.Root {
		return xref.Location{}, false
	}
	if info.Line > math.MaxUint32 {
		return xref.Location{}, false
	}
	if _, err := os.Stat(info.Filename); err != nil {
		return xref.Location{}, false
	}
	if info.Line <= 0 || int64(info.Line) > math.MaxUint32 {
		return xref.Location{}, false
	}
	// Column 1 for both Col and EndCol: export data does not preserve
	// column information (see internal/xref.methodFuncLocation's doc), so
	// this degrades to a zero-width location at the start of the
	// declaration's line rather than guessing.
	return xref.Location{File: info.Filename, Line: uint32(info.Line), Col: 1, EndCol: 1}, true
}

// importDefinition is definitionFallback's path for the cursor being inside
// an import spec's path string (e.g. the quoted "encoding/json"): facts
// extraction never indexes an *ast.ImportSpec (see
// langfeat.ImportPathDefinition's doc), and neither SamePackageDefinition
// nor DependencyDefinition finds an *ast.Ident there to resolve, so without
// this the query answers nothing at all -- the gap this exists to close.
//
// Per gopls, "Go to Definition" on an import path jumps into the imported
// package. internal/graph's Snapshot already has every package in the
// workspace's transitive import graph -- root, module dependency, AND
// standard library alike, since internal/graph's loadMode requests
// NeedFiles for the whole closure, not just root packages -- with real,
// on-disk Go files, so (unlike documentLinkTarget in handlers_nav.go, which
// substitutes a pkg.go.dev URL for anything outside the workspace)
// resolving an import path here never needs that same distinction: any
// package the graph loaded degrades only when it has no Go files
// (unsafe/builtin, or a load failure), not by origin.
//
// gopls itself returns one location per file of the resolved package (see
// golang.org/x/tools/gopls's importDefinition); this instead points at just
// the package's first Go file's package-clause identifier -- the same
// single-file simplification handlers_codeaction.go's packageNameOf
// already makes for the identical "read a package's own declared name"
// need. A definition result needs one always-present, unambiguous
// location, and this still lands the cursor inside the target package,
// without a query-time parse of every one of its files for marginal
// benefit.
func (s *Server) importDefinition(cf checkedFileResult) (xref.Location, bool) {
	pkgPath, ok := langfeat.ImportPathDefinition(cf.cp, cf.path, cf.offset)
	if !ok {
		return xref.Location{}, false
	}
	ws := s.workspace()
	if ws == nil {
		return xref.Location{}, false
	}
	pkg, ok := ws.snap.Package(pkgPath)
	if !ok || len(pkg.GoFiles) == 0 {
		return xref.Location{}, false
	}
	return packageClauseLocation(pkg.GoFiles[0])
}

// packageClauseLocation parses file's package clause fresh from disk (like
// packageNameOf in handlers_codeaction.go) and returns a Location for its
// package name identifier -- e.g. the "io" in "package io" -- rather than
// the bare "package" keyword, consistent with every other Location this
// package returns pointing at a name span, not a keyword.
func packageClauseLocation(file string) (xref.Location, bool) {
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, file, nil, parser.PackageClauseOnly)
	if err != nil || astFile.Name == nil {
		return xref.Location{}, false
	}
	start := fset.Position(astFile.Name.Pos())
	end := fset.Position(astFile.Name.End())
	if start.Line <= 0 || start.Line > math.MaxUint32 || start.Column <= 0 || end.Column <= 0 {
		return xref.Location{}, false
	}
	return xref.Location{
		File:   file,
		Line:   uint32(start.Line),
		Col:    uint32(start.Column),
		EndCol: uint32(end.Column),
	}, true
}

func (s *Server) handleReferences(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.ReferenceParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	locs, err := resolver.References(ctx, path, line, col, p.Context.IncludeDeclaration)
	if err != nil {
		// See handleDefinition's comment: most errors here are an ordinary
		// "no symbol at this position" miss, but log it anyway so a
		// genuine facts-read failure does not vanish silently.
		s.logger.Printf("server: references at %s:%d:%d: %v", path, line, col, err)
		return protocol.LocationSlice(nil), nil
	}
	return s.toLSPLocations(locs), nil
}

func (s *Server) handleImplementation(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.ImplementationParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	locs, err := resolver.Implementation(ctx, path, line, col)
	if err != nil {
		// See handleDefinition's comment: most errors here are an ordinary
		// "no symbol at this position" miss, but log it anyway so a
		// genuine facts-read failure does not vanish silently.
		s.logger.Printf("server: implementation at %s:%d:%d: %v", path, line, col, err)
		return protocol.LocationSlice(nil), nil
	}
	return s.toLSPLocations(locs), nil
}

func (s *Server) handleWorkspaceSymbol(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.WorkspaceSymbolParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return protocol.SymbolInformationSlice(nil), nil
	}
	infos, err := resolver.WorkspaceSymbol(ctx, p.Query)
	if err != nil {
		// Unlike Definition/References/Implementation, a WorkspaceSymbol
		// error is never an ordinary "nothing at this position" miss (it
		// takes no position at all) — it always means a genuine DB lookup
		// failure, so it is always worth logging.
		s.logger.Printf("server: workspace symbol %q: %v", p.Query, err)
		return protocol.SymbolInformationSlice(nil), nil
	}
	out := make(protocol.SymbolInformationSlice, 0, len(infos))
	for _, info := range infos {
		loc, ok := s.correctResultLocation(info.Location)
		if !ok {
			continue
		}
		out = append(out, protocol.SymbolInformation{
			BaseSymbolInformation: protocol.BaseSymbolInformation{
				Name:          info.Name,
				Kind:          workspaceSymbolKind(info.Kind),
				ContainerName: &info.Container,
			},
			Location: loc,
		})
	}
	return out, nil
}

func (s *Server) handleRename(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.RenameParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return nil, rpc.NewError(int32(protocol.ErrorCodesInternalError), "golance: the workspace index is still building; rename is unavailable until it completes")
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return nil, rpc.NewError(int32(protocol.ErrorCodesInvalidRequest), "golance: no renameable symbol at this position")
	}
	edits, err := resolver.Rename(ctx, path, line, col, p.NewName)
	if err != nil {
		// Most errors here mean "no symbol at this position" or a facts-read
		// gap — a routine outcome its sibling handlers (handleDefinition et
		// al.) already treat as an empty result, not a protocol error. Log
		// it so a genuine fault is still visible, rather than surfacing the
		// raw internal error text to the client.
		s.logger.Printf("server: rename at %s:%d:%d: %v", path, line, col, err)
		return nil, nil
	}

	if dirty := s.dirtyRenameFiles(edits); len(dirty) > 0 {
		// correctResultRange's dirty-buffer correction (see dirty.go) only
		// shifts line numbers via a naive top-down line diff and is blind to
		// column-level edits on the same line, so it can silently drop or
		// misplace occurrences. That is an acceptable simplification for
		// its other, read-only callers (definition/references/workspace
		// symbol: worst case a stale result the user re-navigates from),
		// but not here, where it would silently corrupt a write. Rather than
		// risk a partially-wrong WorkspaceEdit, refuse the whole rename
		// loudly whenever any file it touches has unsaved edits.
		msg := "golance: cannot safely rename while " + strings.Join(dirty, ", ") + " has unsaved edits; save and retry"
		s.logger.Printf("server: rename %q: refusing, unsaved edits could shift occurrence positions in %v", p.NewName, dirty)
		return nil, rpc.NewError(int32(protocol.ErrorCodesInternalError), msg)
	}

	changes := make(map[uri.URI][]protocol.TextEdit, len(edits))
	for file, fes := range edits {
		var out []protocol.TextEdit
		for _, e := range fes {
			rng, ok := s.correctResultRange(file, e.Line, e.Col, e.EndCol)
			if !ok {
				continue
			}
			out = append(out, protocol.TextEdit{Range: rng, NewText: e.NewText})
		}
		if len(out) > 0 {
			changes[uri.File(file)] = out
		}
	}
	return &protocol.WorkspaceEdit{Changes: changes}, nil
}
