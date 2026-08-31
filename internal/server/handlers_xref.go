package server

import (
	"context"
	"encoding/json"
	"math"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

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
		return protocol.LocationSlice(nil), nil
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
		return protocol.LocationSlice(nil), nil
	}
	return s.toLSPLocations(locs), nil
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
