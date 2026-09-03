package server

// This file implements call hierarchy (textDocument/prepareCallHierarchy,
// callHierarchy/incomingCalls, callHierarchy/outgoingCalls).
//
// prepareCallHierarchy and outgoingCalls both resolve through
// checkedFile/resolveCheckedPackage -- the same on-demand, single-package
// type-check engine hover/completion already use -- so they see unsaved
// overlay edits and never require the workspace facts index to be built.
// incomingCalls instead answers from the persisted reverse reference index
// (internal/xref.Resolver.References, built over internal/store's
// PostingsFor postings), the same machinery textDocument/references uses,
// and so shares its readiness contract: while the index is still building,
// it answers indexUnavailableError (LSPErrorCodesRequestFailed) rather than
// an empty result -- see resolverOrWarn's doc for why an empty result is
// unsafe there.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"os"
	"path/filepath"
	"sort"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"golang.org/x/tools/go/ast/astutil"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/xref"
)

// callHierarchyTarget resolves an LSP TextDocumentPositionParams-shaped
// request (u, pos) to the *types.Func at the cursor, via checkedFile and
// langfeat.CallHierarchyFuncAt. Both handlePrepareCallHierarchy (from the
// query position) and handleOutgoingCalls (from the item's own declaring
// position, re-resolving what prepare/incoming already found) share this.
func (s *Server) callHierarchyTarget(ctx context.Context, u uri.URI, pos protocol.Position) (checkedFileResult, *types.Func, bool) {
	cf := s.checkedFile(ctx, u, pos)
	if !cf.ok {
		return cf, nil, false
	}
	fn, ok := langfeat.CallHierarchyFuncAt(cf.cp, cf.path, cf.offset)
	if !ok {
		return cf, nil, false
	}
	return cf, fn, true
}

// callHierarchyItem builds the protocol.CallHierarchyItem for fn, resolved
// against cp: same-package via cp's own AST (langfeat.FuncDeclaration),
// cross-package through the workspace facts index or internal/depcheck
// (crossPackageFuncLocation) -- the identical chain
// typeDefinitionCrossPackage uses for a named type's declaration, applied
// here to a func/method's own declaration instead.
func (s *Server) callHierarchyItem(ctx context.Context, cp *check.CheckedPackage, fn *types.Func) (protocol.CallHierarchyItem, bool) {
	info, err := langfeat.FuncDeclaration(cp, fn)
	if err != nil {
		s.logger.Printf("server: call hierarchy declaration for %s: %v", fn.Name(), err)
		return protocol.CallHierarchyItem{}, false
	}
	if info == nil {
		return protocol.CallHierarchyItem{}, false
	}

	var loc protocol.Location
	var ok bool
	if info.SameFile != "" {
		loc, ok = s.sameFileCallHierarchyLocation(info.SameFile, info.Range)
	} else {
		loc, ok = s.crossPackageFuncLocation(ctx, info.PkgPath, info.ObjPath)
	}
	if !ok {
		return protocol.CallHierarchyItem{}, false
	}

	detail := callHierarchyDetail(fn.Pkg().Path(), loc.URI.FsPath())
	return protocol.CallHierarchyItem{
		Name:           fn.Name(),
		Kind:           protocol.SymbolKindFunction,
		Detail:         &detail,
		URI:            loc.URI,
		Range:          loc.Range,
		SelectionRange: loc.Range,
	}, true
}

// sameFileCallHierarchyLocation converts a same-package FuncDeclInfo (byte
// offsets against file's own current buffer) into an LSP location, the same
// pattern typeDefinitionSameFile/samePackageDefinitionLocation already use.
func (s *Server) sameFileCallHierarchyLocation(file string, r langfeat.Range) (protocol.Location, bool) {
	text, err := s.overlay.ReadFile(file)
	if err != nil {
		s.logger.Printf("server: call hierarchy declaration read %s: %v", file, err)
		return protocol.Location{}, false
	}
	rng, ok := offsetRangeToLSP(text, r.StartOffset, r.EndOffset)
	if !ok {
		return protocol.Location{}, false
	}
	return protocol.Location{URI: uri.File(file), Range: rng}, true
}

// crossPackageFuncLocation resolves the func/method identified by
// (pkgPath, objPath) to an LSP Location: through the workspace facts index
// when pkgPath is a root package (resolver.TypeDeclaration, despite its
// name, is a plain (pkgPath, objPath) -> Location lookup with no
// type-specific behavior -- the same one typeDefinitionCrossPackage already
// uses for a named type), falling back to ws.depProvider for a standard
// library or module dependency package otherwise.
func (s *Server) crossPackageFuncLocation(ctx context.Context, pkgPath, objPath string) (protocol.Location, bool) {
	if resolver, ok := s.resolverOrWarn(); ok {
		if loc, ok := resolver.TypeDeclaration(ctx, pkgPath, objPath); ok {
			if pl, ok := s.correctResultLocation(loc); ok {
				return pl, true
			}
		}
	}
	return s.dependencyFuncDeclaration(ctx, pkgPath, objPath)
}

// dependencyFuncDeclaration is crossPackageFuncLocation's fallback for a
// func/method declared in a standard library or module dependency package,
// mirroring dependencyTypeDeclaration's identical mechanism and validation
// for a named type's declaration.
func (s *Server) dependencyFuncDeclaration(ctx context.Context, pkgPath, objPath string) (protocol.Location, bool) {
	ws := s.workspace()
	if ws == nil {
		return protocol.Location{}, false
	}
	if pkg, ok := ws.snap.Packages[pkgPath]; ok && pkg.Root {
		return protocol.Location{}, false
	}
	id, fset, err := ws.depProvider.DeclAt(ctx, pkgPath, objPath)
	if err != nil {
		s.logger.Printf("server: dependency call hierarchy declaration %s#%s: %v", pkgPath, objPath, err)
		return protocol.Location{}, false
	}
	start := fset.Position(id.Pos())
	end := fset.Position(id.End())
	if !validExportPosition(start, end) {
		return protocol.Location{}, false
	}
	loc := xref.Location{File: start.Filename, Line: uint32(start.Line), Col: uint32(start.Column), EndCol: uint32(end.Column)}
	return s.correctResultLocation(loc)
}

// validExportPosition reports whether start/end -- a declaring identifier's
// span resolved through depcheck's own FileSet -- names a real, readable
// file with line/column values that fit xref.Location's uint32 fields.
// Mirrors the identical validation dependencyDefinition and
// dependencyTypeDeclaration each already inline for their own depcheck
// results (handlers_xref.go/handlers_nav.go).
func validExportPosition(start, end token.Position) bool {
	if _, err := os.Stat(start.Filename); err != nil {
		return false
	}
	if start.Line <= 0 || int64(start.Line) > math.MaxUint32 {
		return false
	}
	if start.Column <= 0 || int64(start.Column) > math.MaxUint32 {
		return false
	}
	if end.Column <= 0 || int64(end.Column) > math.MaxUint32 {
		return false
	}
	return true
}

// handlePrepareCallHierarchy answers textDocument/prepareCallHierarchy: the
// enclosing function/method at the cursor, per gopls's own
// callHierarchyFuncAtRange -- any identifier resolving to a *types.Func,
// whether it names a declaration or a call site.
func (s *Server) handlePrepareCallHierarchy(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.CallHierarchyPrepareParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf, fn, ok := s.callHierarchyTarget(ctx, p.TextDocument.URI, p.Position)
	if !ok {
		return []protocol.CallHierarchyItem(nil), nil
	}
	item, ok := s.callHierarchyItem(ctx, cf.cp, fn)
	if !ok {
		return []protocol.CallHierarchyItem(nil), nil
	}
	return []protocol.CallHierarchyItem{item}, nil
}

// handleOutgoingCalls answers callHierarchy/outgoingCalls: every call fn's
// own declaration makes, aggregated by callee (langfeat.OutgoingCalls),
// resolved to protocol.CallHierarchyOutgoingCall via the same
// callHierarchyItem chain prepare uses for the queried item itself.
func (s *Server) handleOutgoingCalls(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.CallHierarchyOutgoingCallsParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf, fn, ok := s.callHierarchyTarget(ctx, p.Item.URI, p.Item.Range.Start)
	if !ok {
		return []protocol.CallHierarchyOutgoingCall(nil), nil
	}
	calls, err := langfeat.OutgoingCalls(cf.cp, fn)
	if err != nil {
		s.logger.Printf("server: outgoing calls %s: %v", cf.path, err)
		return []protocol.CallHierarchyOutgoingCall(nil), nil
	}

	out := make([]protocol.CallHierarchyOutgoingCall, 0, len(calls))
	for _, c := range calls {
		item, ok := s.callHierarchyItem(ctx, cf.cp, c.Callee)
		if !ok {
			continue
		}
		ranges := make([]protocol.Range, 0, len(c.FromRanges))
		for _, r := range c.FromRanges {
			if rng, ok := offsetRangeToLSP(cf.text, r.StartOffset, r.EndOffset); ok {
				ranges = append(ranges, rng)
			}
		}
		if len(ranges) == 0 {
			continue
		}
		out = append(out, protocol.CallHierarchyOutgoingCall{To: item, FromRanges: ranges})
	}
	sortCallHierarchyItems(out, func(i int) protocol.Location {
		return protocol.Location{URI: out[i].To.URI, Range: out[i].To.Range}
	})
	return out, nil
}

// handleIncomingCalls answers callHierarchy/incomingCalls: every reference
// to the queried item's own symbol (resolver.References, the same reverse
// reference index textDocument/references uses -- including its
// interface/concrete-method union, so a call reached only through an
// interface a concrete method satisfies is included, matching gopls's own
// IncomingCalls), folded into each reference's own enclosing function
// declaration (foldIncomingCalls).
func (s *Server) handleIncomingCalls(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.CallHierarchyIncomingCallsParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return nil, s.indexUnavailableError("call hierarchy incoming calls")
	}
	path := p.Item.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Item.Range.Start)
	if !ok {
		return []protocol.CallHierarchyIncomingCall(nil), nil
	}
	locs, err := resolver.References(ctx, path, line, col, false)
	if err != nil {
		s.logger.Printf("server: call hierarchy incoming calls at %s:%d:%d: %v", path, line, col, err)
		return []protocol.CallHierarchyIncomingCall(nil), nil
	}
	return s.foldIncomingCalls(ctx, locs), nil
}

// chSourceFile is foldIncomingCalls' per-file cache entry: text and its
// parsed AST (current overlay content if open, disk otherwise -- see
// s.overlay.ReadFile), plus the dirty-buffer line correction inputs
// (correctResultRange's identical dirtyLines/dirtyLineMap pair) computed
// once and reused for every reference the file contributes, rather than
// once per reference the way toLSPLocations' own correctResultLocation
// calls do for a plain (non-folding) references result.
type chSourceFile struct {
	text         []byte
	fset         *token.FileSet
	astFile      *ast.File // nil if file could not be parsed at all
	pkgPath      string
	saved        []byte
	dirty        []byte
	dirtyLinesOK bool
}

// foldIncomingCalls converts locs (raw reference locations from
// resolver.References) into gopls's own IncomingCalls shape: one entry per
// distinct enclosing function declaration, with FromRanges collecting every
// reference inside it (enclosingCallItem's dedup key), sorted by that
// declaration's own location for a deterministic result order. It parses
// each distinct referencing file at most once (cached in files), matching
// O(result files) rather than O(results).
func (s *Server) foldIncomingCalls(ctx context.Context, locs []xref.Location) []protocol.CallHierarchyIncomingCall {
	files := make(map[string]*chSourceFile)
	calls := make(map[protocol.Location]*protocol.CallHierarchyIncomingCall)
	var order []protocol.Location

	for _, loc := range locs {
		if err := ctx.Err(); err != nil {
			break
		}
		sf := s.chSourceFileFor(loc.File, files)
		if sf == nil || sf.astFile == nil {
			continue
		}
		line := loc.Line
		if sf.dirtyLinesOK {
			if mapped, ok := dirtyLineMap(sf.saved, sf.dirty, line); ok {
				line = mapped
			}
		}
		fromRange, ok := xrefRangeToLSP(sf.text, line, loc.Col, loc.EndCol)
		if !ok {
			continue
		}
		offset, ok := byteOffsetForPosition(sf.text, fromRange.Start)
		if !ok {
			continue
		}
		item, ok := enclosingCallItem(sf.fset, sf.astFile, sf.text, loc.File, sf.pkgPath, offset)
		if !ok {
			continue
		}

		key := protocol.Location{URI: item.URI, Range: item.Range}
		call, exists := calls[key]
		if !exists {
			call = &protocol.CallHierarchyIncomingCall{From: item}
			calls[key] = call
			order = append(order, key)
		}
		call.FromRanges = append(call.FromRanges, fromRange)
	}

	sort.Slice(order, func(i, j int) bool { return compareLocation(order[i], order[j]) })
	out := make([]protocol.CallHierarchyIncomingCall, 0, len(order))
	for _, key := range order {
		out = append(out, *calls[key])
	}
	return out
}

// chSourceFileFor returns path's cached chSourceFile, parsing it (via the
// current overlay content, dirty-corrected against its on-disk facts-index
// content if it has unsaved edits) on first use.
func (s *Server) chSourceFileFor(path string, cache map[string]*chSourceFile) *chSourceFile {
	if sf, ok := cache[path]; ok {
		return sf
	}
	saved, dirty, dirtyOK := s.dirtyLines(path)
	text := dirty
	if !dirtyOK {
		t, err := s.overlay.ReadFile(path)
		if err != nil {
			cache[path] = nil
			return nil
		}
		text = t
	}
	fset := token.NewFileSet()
	astFile, _ := parser.ParseFile(fset, path, text, 0)
	pkgPath, _ := s.pkgPathForFile(path)
	sf := &chSourceFile{text: text, fset: fset, astFile: astFile, pkgPath: pkgPath, saved: saved, dirty: dirty, dirtyLinesOK: dirtyOK}
	cache[path] = sf
	return sf
}

// enclosingCallItem builds the CallHierarchyItem representing the function
// call at offset (a byte offset into text, astFile's parsed content):
// gopls's own enclosingNodeCallItem algorithm (golang.org/x/tools/gopls's
// internal/golang/call_hierarchy.go) walking astutil.PathEnclosingInterval
// from offset's innermost enclosing node outward, letting each of
// *ast.FuncDecl, *ast.FuncLit, and *ast.ValueSpec overwrite the previous
// match in turn -- so the OUTERMOST of these ever seen wins. A named
// function's own FuncDecl always wins over any FuncLit nested inside it
// (gopls folds a named function and every literal nested inside it into one
// call hierarchy entity), and a FuncLit wins over an inner ValueSpec (a
// local var's own initializer), because each is only ever overwritten by a
// node found LATER in the innermost-to-outermost walk.
// Only a call with no enclosing FuncDecl/FuncLit/ValueSpec at all (which
// cannot happen for real Go source: every statement lives inside some file-
// level declaration) keeps the package-level fallback this starts from.
func enclosingCallItem(fset *token.FileSet, astFile *ast.File, text []byte, filePath, pkgPath string, offset int) (protocol.CallHierarchyItem, bool) {
	tf := fset.File(astFile.Pos())
	if tf == nil || offset < 0 || offset > tf.Size() {
		return protocol.CallHierarchyItem{}, false
	}
	pos := tf.Pos(offset)
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)

	name := astFile.Name.Name
	kind := protocol.SymbolKindPackage
	start, end := astFile.Name.Pos(), astFile.Name.End()

	for _, n := range path {
		switch node := n.(type) {
		case *ast.FuncDecl:
			name = node.Name.Name
			start, end = node.Name.Pos(), node.Name.End()
			kind = protocol.SymbolKindFunction
		case *ast.FuncLit:
			name = "func"
			start, end = node.Pos(), node.Type.End()
			kind = protocol.SymbolKindFunction
		case *ast.ValueSpec:
			name = "init"
			start, end = node.Names[0].Pos(), node.Names[len(node.Names)-1].End()
			kind = protocol.SymbolKindVariable
		}
	}

	rng, ok := offsetRangeToLSP(text, tf.Offset(start), tf.Offset(end))
	if !ok {
		return protocol.CallHierarchyItem{}, false
	}

	detail := callHierarchyDetail(pkgPath, filePath)
	return protocol.CallHierarchyItem{
		Name:           name,
		Kind:           kind,
		Detail:         &detail,
		URI:            uri.File(filePath),
		Range:          rng,
		SelectionRange: rng,
	}, true
}

// callHierarchyDetail builds a CallHierarchyItem.Detail string in gopls's
// own "pkgPath • basename" shape (see gopls's callHierarchyItemDetail),
// omitting the package segment when pkgPath is unknown (enclosingCallItem's
// package-level fallback, when s.pkgPathForFile could not resolve one).
func callHierarchyDetail(pkgPath, filePath string) string {
	base := filepath.Base(filePath)
	if pkgPath == "" {
		return base
	}
	return pkgPath + " • " + base
}

// compareLocation orders two protocol.Location values by URI then by
// range start, for foldIncomingCalls'/sortCallHierarchyItems' deterministic
// result ordering.
func compareLocation(a, b protocol.Location) bool {
	if a.URI != b.URI {
		return a.URI < b.URI
	}
	if a.Range.Start.Line != b.Range.Start.Line {
		return a.Range.Start.Line < b.Range.Start.Line
	}
	return a.Range.Start.Character < b.Range.Start.Character
}

// sortCallHierarchyItems sorts items in place by the protocol.Location
// locAt(i) reports for each index i, via compareLocation.
func sortCallHierarchyItems(items []protocol.CallHierarchyOutgoingCall, locAt func(i int) protocol.Location) {
	sort.Slice(items, func(i, j int) bool { return compareLocation(locAt(i), locAt(j)) })
}
