package server

// This file implements type hierarchy (textDocument/prepareTypeHierarchy,
// typeHierarchy/supertypes, typeHierarchy/subtypes).
//
// prepareTypeHierarchy type-checks a single package (the same checkedFile
// machinery hover/completion/prepareCallHierarchy use), so it runs
// Interactive and sees unsaved overlay edits without needing the workspace
// facts index built -- see handlers_callhierarchy.go's identical rationale
// for prepareCallHierarchy. supertypes/subtypes instead answer from the
// persisted facts index (internal/xref.Resolver.Supertypes/Subtypes, built
// over the same method postings textDocument/implementation's
// Implementation query already uses), so they run Background and share its
// readiness contract: while the index is still building, they answer
// indexUnavailableError (LSPErrorCodesRequestFailed) rather than an empty
// result -- see resolverOrWarn's doc for why an empty result is unsafe
// there.

import (
	"context"
	"encoding/json"
	"sort"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/xref"
)

// handlePrepareTypeHierarchy answers textDocument/prepareTypeHierarchy: the
// type name at the cursor, resolved the same on-demand way
// prepareCallHierarchy resolves a func/method (per gopls's own
// PrepareTypeHierarchy, only a literal type name qualifies -- see
// langfeat.TypeHierarchyPrepare's doc).
func (s *Server) handlePrepareTypeHierarchy(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.TypeHierarchyPrepareParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if !cf.ok {
		return []protocol.TypeHierarchyItem(nil), nil
	}
	info, err := langfeat.TypeHierarchyPrepare(cf.cp, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: prepare type hierarchy %s: %v", cf.path, err)
		return []protocol.TypeHierarchyItem(nil), nil
	}
	if info == nil {
		return []protocol.TypeHierarchyItem(nil), nil
	}
	item, ok := s.typeHierarchyItemFromPrepare(ctx, info)
	if !ok {
		return []protocol.TypeHierarchyItem(nil), nil
	}
	return []protocol.TypeHierarchyItem{item}, nil
}

// typeHierarchyItemFromPrepare resolves a TypeHierarchyPrepareInfo to its
// LSP item: same-package via cf's own overlay text
// (sameFileCallHierarchyLocation, which is location-shape-agnostic despite
// its call-hierarchy name), cross-package through the workspace facts index
// or internal/depcheck (crossPackageFuncLocation, likewise a plain
// (pkgPath, objPath) -> Location lookup with no func-specific behavior) --
// the identical chain handlers_callhierarchy.go's callHierarchyItem already
// uses for a func/method's own declaration, reused here for a type's.
func (s *Server) typeHierarchyItemFromPrepare(ctx context.Context, info *langfeat.TypeHierarchyPrepareInfo) (protocol.TypeHierarchyItem, bool) {
	var loc protocol.Location
	var ok bool
	if info.SameFile != "" {
		loc, ok = s.sameFileCallHierarchyLocation(info.SameFile, info.Range)
	} else {
		loc, ok = s.crossPackageFuncLocation(ctx, info.PkgPath, info.ObjPath)
	}
	if !ok {
		return protocol.TypeHierarchyItem{}, false
	}
	return typeHierarchyItem(info.Name, info.IsInterface, info.PkgPath, loc), true
}

// handleTypeHierarchySupertypes answers typeHierarchy/supertypes.
func (s *Server) handleTypeHierarchySupertypes(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.TypeHierarchySupertypesParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	return s.relatedTypeHierarchy(ctx, &p.Item, "supertypes", (*xref.Resolver).Supertypes)
}

// handleTypeHierarchySubtypes answers typeHierarchy/subtypes.
func (s *Server) handleTypeHierarchySubtypes(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.TypeHierarchySubtypesParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	return s.relatedTypeHierarchy(ctx, &p.Item, "subtypes", (*xref.Resolver).Subtypes)
}

// relatedTypeHierarchy answers typeHierarchy/supertypes and
// typeHierarchy/subtypes alike: resolve item's own declaring position back
// through the facts index via query (xref.Resolver.Supertypes or .Subtypes),
// then build a protocol.TypeHierarchyItem for each result, sorted the same
// way gopls's own relatedTypes orders its result (the queried item's own
// package first, then name, URI, range) -- see typeHierarchyLess' doc for
// why no further de-duplication is needed here, unlike gopls's own
// CompactFunc pass.
func (s *Server) relatedTypeHierarchy(ctx context.Context, item *protocol.TypeHierarchyItem, feature string, query func(*xref.Resolver, context.Context, string, int, int) ([]xref.TypeHierarchyItemInfo, error)) (any, error) {
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return nil, s.indexUnavailableError("type hierarchy " + feature)
	}
	path := item.URI.FsPath()
	line, col, ok := s.xrefPosition(path, item.Range.Start)
	if !ok {
		return []protocol.TypeHierarchyItem(nil), nil
	}
	infos, err := query(resolver, ctx, path, line, col)
	if err != nil {
		s.logger.Printf("server: type hierarchy %s at %s:%d:%d: %v", feature, path, line, col, err)
		return []protocol.TypeHierarchyItem(nil), nil
	}
	out := make([]protocol.TypeHierarchyItem, 0, len(infos))
	for _, info := range infos {
		loc, ok := s.correctResultLocation(info.Location)
		if !ok {
			continue
		}
		out = append(out, typeHierarchyItem(info.Name, info.IsInterface, info.PkgPath, loc))
	}
	ownDetail := ""
	if item.Detail != nil {
		ownDetail = *item.Detail
	}
	sort.Slice(out, func(i, j int) bool { return typeHierarchyLess(out, i, j, ownDetail) })
	return out, nil
}

// typeHierarchyItem builds a protocol.TypeHierarchyItem for name, in
// gopls's own shape: Detail is just the defining package's import path
// (Kind Class for a concrete type, Interface for an interface -- matching
// gopls's own PrepareTypeHierarchy/relatedTypes, and deliberately NOT
// golance's own workspaceSymbolKind convention of mapping a concrete named
// type to SymbolKindStruct: a type hierarchy view's icon set is Class vs
// Interface, per the LSP spec's own SymbolKind semantics gopls follows).
func typeHierarchyItem(name string, isInterface bool, pkgPath string, loc protocol.Location) protocol.TypeHierarchyItem {
	detail := pkgPath
	kind := protocol.SymbolKindClass
	if isInterface {
		kind = protocol.SymbolKindInterface
	}
	return protocol.TypeHierarchyItem{
		Name:           name,
		Kind:           kind,
		Detail:         &detail,
		URI:            loc.URI,
		Range:          loc.Range,
		SelectionRange: loc.Range,
	}
}

// typeHierarchyLess orders items[i] and items[j] by (package, name, URI,
// range), mirroring gopls's own relatedTypes sort (golang.org/x/tools/
// gopls's type_hierarchy.go), ranking a result in the SAME package as the
// queried item (ownDetail) ahead of every other package. protocol.
// TypeHierarchyItem is a 128-byte struct, so this takes items and a pair of
// indices rather than two items by value, avoiding a copy per comparison
// (gocritic's hugeParam/rangeValCopy). Unlike relatedTypes, this package
// deliberately does not follow with a CompactFunc de-duplication pass:
// gopls's own Supertypes/Subtypes can report the same type twice because it
// searches the query's OWN package via one algorithm (a live AST scan,
// localImplementations) and every workspace package -- including that very
// same one -- via a second, independent one (the global methodsets index),
// so a query package's own type can surface from both. golance's
// xref.Resolver.Supertypes/Subtypes is a single, index-only algorithm whose
// candidate map is already keyed by (package, type) -- see
// internal/xref/typehierarchy.go's candidateKey -- so this structural
// double-counting cannot occur here at all.
func typeHierarchyLess(items []protocol.TypeHierarchyItem, i, j int, ownDetail string) bool {
	a, b := &items[i], &items[j]
	da, db := detailOf(a), detailOf(b)
	if da != db {
		ownA, ownB := da == ownDetail, db == ownDetail
		if ownA != ownB {
			return ownA
		}
		return da < db
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.URI != b.URI {
		return a.URI < b.URI
	}
	return positionBefore(a.Range.Start, b.Range.Start)
}

func detailOf(item *protocol.TypeHierarchyItem) string {
	if item.Detail == nil {
		return ""
	}
	return *item.Detail
}
