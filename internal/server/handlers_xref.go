package server

import (
	"context"
	"encoding/json"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/xref"
)

// resolverOrWarn returns the current facts-index Resolver, or ok=false if
// the indexer subprocess has not completed a build yet. On the first such
// call it warns the client once via window/showMessage; later calls stay
// silent so a burst of queries during index build does not spam the user.
func (s *Server) resolverOrWarn() (*xref.Resolver, bool) {
	idx := s.idx.Load()
	if idx == nil {
		if s.indexBuildingWarned.CompareAndSwap(false, true) {
			s.showMessage(protocol.MessageTypeInfo, "golance: the workspace index is still building; cross-reference results are unavailable until it completes")
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

func (s *Server) handleDefinition(_ context.Context, params json.RawMessage) (any, error) {
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
	locs, err := resolver.Definition(path, line, col)
	if err != nil {
		return protocol.LocationSlice(nil), nil
	}
	return s.toLSPLocations(locs), nil
}

func (s *Server) handleReferences(_ context.Context, params json.RawMessage) (any, error) {
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
	locs, err := resolver.References(path, line, col, p.Context.IncludeDeclaration)
	if err != nil {
		return protocol.LocationSlice(nil), nil
	}
	return s.toLSPLocations(locs), nil
}

func (s *Server) handleImplementation(_ context.Context, params json.RawMessage) (any, error) {
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
	locs, err := resolver.Implementation(path, line, col)
	if err != nil {
		return protocol.LocationSlice(nil), nil
	}
	return s.toLSPLocations(locs), nil
}

func (s *Server) handleWorkspaceSymbol(_ context.Context, params json.RawMessage) (any, error) {
	var p protocol.WorkspaceSymbolParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return protocol.SymbolInformationSlice(nil), nil
	}
	infos, err := resolver.WorkspaceSymbol(p.Query)
	if err != nil {
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

func (s *Server) handleRename(_ context.Context, params json.RawMessage) (any, error) {
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
	edits, err := resolver.Rename(path, line, col, p.NewName)
	if err != nil {
		return nil, rpc.NewError(int32(protocol.ErrorCodesInvalidRequest), err.Error())
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
