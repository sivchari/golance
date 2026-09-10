package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"golang.org/x/tools/go/types/objectpath"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/diff"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/xref"
)

// registerNavHandlers registers the navigation and light editing-support LSP
// methods this file implements: typeDefinition, declaration,
// documentHighlight, prepareRename, foldingRange, selectionRange,
// rangeFormatting, documentLink, textDocument/completion, and
// completionItem/resolve.
func (s *Server) registerNavHandlers() {
	s.rpc.Handle(protocol.MethodTextDocumentTypeDefinition, rpc.Background, s.handleTypeDefinition)
	s.rpc.Handle(protocol.MethodTextDocumentDeclaration, rpc.Background, s.handleDeclaration)
	s.rpc.Handle(protocol.MethodTextDocumentDocumentHighlight, rpc.Interactive, s.handleDocumentHighlight)
	s.rpc.Handle(protocol.MethodTextDocumentPrepareRename, rpc.Background, s.handlePrepareRename)
	s.rpc.Handle(protocol.MethodTextDocumentFoldingRange, rpc.Interactive, s.handleFoldingRange)
	s.rpc.Handle(protocol.MethodTextDocumentSelectionRange, rpc.Interactive, s.handleSelectionRange)
	s.rpc.Handle(protocol.MethodTextDocumentRangeFormatting, rpc.Interactive, s.handleDocumentRangeFormatting)
	s.rpc.Handle(protocol.MethodTextDocumentDocumentLink, rpc.Interactive, s.handleDocumentLink)
	s.rpc.Handle(protocol.MethodCompletionItemResolve, rpc.Interactive, s.handleCompletionResolve)

	// handleCompletionWithData wraps handleCompletion, embedding enough Data
	// in each item for completionItem/resolve (registered just above) to
	// fill in Documentation lazily instead of eagerly for every candidate
	// (see internal/langfeat.ResolveCompletionDoc). It is the sole
	// registration for textDocument/completion; handleCompletion itself is
	// not registered directly.
	s.rpc.Handle(protocol.MethodTextDocumentCompletion, rpc.Interactive, s.handleCompletionWithData)
}

// handleTypeDefinition answers textDocument/typeDefinition: the declaration
// of the named type of the identifier at the cursor.
func (s *Server) handleTypeDefinition(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.TypeDefinitionParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if !cf.ok {
		return protocol.LocationSlice(nil), nil
	}
	info, err := langfeat.TypeDefinition(cf.cp, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: type definition %s: %v", cf.path, err)
		return protocol.LocationSlice(nil), nil
	}
	if info == nil {
		return protocol.LocationSlice(nil), nil
	}
	if info.SameFile != "" {
		return s.typeDefinitionSameFile(info)
	}
	if info.Builtin != nil {
		return s.typeDefinitionBuiltin(info.Builtin)
	}
	return s.typeDefinitionCrossPackage(ctx, info)
}

// typeDefinitionBuiltin converts a predeclared named type's BuiltinDefInfo
// (TypeDefInfo.Builtin, langfeat.TypeDefinition's resolution for e.g.
// error or int) into an LSP location in builtin.go, reusing
// builtinDefLocation -- the same conversion handleDefinition's own
// builtinDefinition fallback uses (handlers_xref.go).
func (s *Server) typeDefinitionBuiltin(info *langfeat.BuiltinDefInfo) (any, error) {
	loc, ok := builtinDefLocation(s.logger, info)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	pl, ok := s.correctResultLocation(loc)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	return protocol.LocationSlice{pl}, nil
}

// typeDefinitionSameFile converts a same-package TypeDefInfo (byte offsets
// against info.SameFile's own current buffer) into an LSP location.
func (s *Server) typeDefinitionSameFile(info *langfeat.TypeDefInfo) (any, error) {
	text, err := s.overlay.ReadFile(info.SameFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logger.Printf("server: type definition read %s: %v", info.SameFile, err)
		}
		return protocol.LocationSlice(nil), nil
	}
	rng, ok := offsetRangeToLSP(text, info.Range.StartOffset, info.Range.EndOffset)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	return protocol.LocationSlice{{URI: uri.File(info.SameFile), Range: rng}}, nil
}

// typeDefinitionCrossPackage resolves a cross-package TypeDefInfo through
// the on-disk facts index, falling back to dependencyTypeDeclaration when
// the index has no answer — the facts index only ever covers root
// (workspace) packages (internal/index/scheduler.go's doc), so a type
// declared in the standard library or a module dependency always misses
// there. Before dependencyTypeDeclaration existed, that miss was a silent
// early return (PR #30's report); this now mirrors definitionFallback's
// identical resolver-then-depProvider chain for plain "Go to Definition".
//
// A root-package target the facts index cannot yet answer because it has
// not finished building (resolverOrWarn's ok=false) is reported via
// indexUnavailableError instead of an empty result, matching references/
// implementation/rename: dependencyTypeDeclaration always declines a root
// package too (see its own doc), so without this distinction such a target
// would silently look identical to one that genuinely has no declaration —
// PR #30's regression recurring in this handler's own cross-package chain.
func (s *Server) typeDefinitionCrossPackage(ctx context.Context, info *langfeat.TypeDefInfo) (any, error) {
	resolver, resolverOK := s.resolverOrWarn()
	if resolverOK {
		if loc, ok := resolver.TypeDeclaration(ctx, info.PkgPath, info.ObjPath); ok {
			if pl, ok := s.correctResultLocation(loc); ok {
				return protocol.LocationSlice{pl}, nil
			}
		}
	}
	if pl, ok := s.dependencyTypeDeclaration(ctx, info); ok {
		return protocol.LocationSlice{pl}, nil
	}
	if !resolverOK && s.isRootPackage(info.PkgPath) {
		return nil, s.indexUnavailableError("type definition")
	}
	return protocol.LocationSlice(nil), nil
}

// isRootPackage reports whether pkgPath names a workspace root package, per
// the current workspace snapshot. dependencyTypeDeclaration/
// dependencyFuncDeclaration already decline to answer for a root package
// (see their own doc), for a reason unrelated to index readiness, so their
// own false return alone cannot tell "the facts index has nothing for this
// identifier" apart from "the facts index is the only possible source for
// this identifier and is not ready yet" — callers that need that
// distinction (typeDefinitionCrossPackage, crossPackageFuncLocation) check
// this directly alongside resolverOrWarn's own ok.
func (s *Server) isRootPackage(pkgPath string) bool {
	ws := s.workspace()
	if ws == nil {
		return false
	}
	pkg, ok := ws.snap.Packages[pkgPath]
	return ok && pkg.Root
}

// dependencyTypeDeclaration is typeDefinitionCrossPackage's fallback for a
// type declared in a standard library or module dependency package:
// resolved through ws.depProvider (internal/depcheck), exact to the column
// and able to see unexported dependency types, mirroring
// dependencyDefinition's identical mechanism for plain "Go to Definition"
// (see its doc, including the same root-package exclusion below and why it
// exists).
func (s *Server) dependencyTypeDeclaration(ctx context.Context, info *langfeat.TypeDefInfo) (protocol.Location, bool) {
	ws := s.workspace()
	if ws == nil {
		return protocol.Location{}, false
	}
	if pkg, ok := ws.snap.Packages[info.PkgPath]; ok && pkg.Root {
		return protocol.Location{}, false
	}
	id, fset, err := ws.depProvider.DeclAt(ctx, info.PkgPath, info.ObjPath)
	if err != nil {
		s.logger.Printf("server: dependency type declaration %s#%s: %v", info.PkgPath, info.ObjPath, err)
		return protocol.Location{}, false
	}
	start := fset.Position(id.Pos())
	end := fset.Position(id.End())
	if _, err := os.Stat(start.Filename); err != nil {
		s.logger.Printf("server: dependency type declaration %s#%s: declaration source %s: %v", info.PkgPath, info.ObjPath, start.Filename, err)
		return protocol.Location{}, false
	}
	if start.Line <= 0 || int64(start.Line) > math.MaxUint32 ||
		start.Column <= 0 || int64(start.Column) > math.MaxUint32 ||
		end.Column <= 0 || int64(end.Column) > math.MaxUint32 {
		return protocol.Location{}, false
	}
	loc := xref.Location{File: start.Filename, Line: uint32(start.Line), Col: uint32(start.Column), EndCol: uint32(end.Column)}
	return s.correctResultLocation(loc)
}

// handleDeclaration answers textDocument/declaration. Go has no separate
// notion of "declaration" versus "definition" (unlike, say, a C header/
// source split), so this is definition under another name: declaration and
// definition requests carry an identical JSON shape (a TextDocumentPosition
// plus progress/partial-result options), so the raw params pass straight
// through to handleDefinition.
func (s *Server) handleDeclaration(ctx context.Context, params json.RawMessage) (any, error) {
	return s.handleDefinition(ctx, params)
}

// handleDocumentHighlight answers textDocument/documentHighlight: every
// occurrence, within the same file, of the symbol at the cursor.
func (s *Server) handleDocumentHighlight(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.DocumentHighlightParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if !cf.ok {
		return []protocol.DocumentHighlight{}, nil
	}
	hs, err := langfeat.DocumentHighlight(cf.cp, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: document highlight %s: %v", cf.path, err)
		return []protocol.DocumentHighlight{}, nil
	}
	out := make([]protocol.DocumentHighlight, 0, len(hs))
	for _, h := range hs {
		rng, ok := offsetRangeToLSP(cf.text, h.Range.StartOffset, h.Range.EndOffset)
		if !ok {
			continue
		}
		out = append(out, protocol.DocumentHighlight{Range: rng, Kind: documentHighlightKind(h.Kind)})
	}
	return out, nil
}

func documentHighlightKind(k langfeat.HighlightKind) protocol.DocumentHighlightKind {
	if k == langfeat.HighlightWrite {
		return protocol.DocumentHighlightKindWrite
	}
	return protocol.DocumentHighlightKindRead
}

// handlePrepareRename answers textDocument/prepareRename: whether the
// identifier at the cursor can be renamed, and if so, its current range.
func (s *Server) handlePrepareRename(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.PrepareRenameParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if !cf.ok {
		return nil, nil
	}
	r, err := langfeat.PrepareRename(cf.cp, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: prepare rename %s: %v", cf.path, err)
		return nil, nil
	}
	if r == nil {
		return nil, nil
	}
	rng, ok := offsetRangeToLSP(cf.text, r.StartOffset, r.EndOffset)
	if !ok {
		return nil, nil
	}
	return &rng, nil
}

// handleFoldingRange answers textDocument/foldingRange.
func (s *Server) handleFoldingRange(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.FoldingRangeParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	ws := s.workspace()
	if ws == nil {
		return []protocol.FoldingRange{}, nil
	}
	cp, err := s.resolveCheckedPackage(ctx, ws, path)
	if err != nil {
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return []protocol.FoldingRange{}, nil
	}
	// FileText, not a separate s.overlay.ReadFile(path): since GraphSource's
	// directory fallback (see internal/check.GraphSource.PackageForFile), a
	// successful Get no longer guarantees path itself is one of cp's files
	// — e.g. a stale path in a known package's directory that was never
	// actually opened or saved. FileText degrades that case to !ok, the
	// same "no result" treatment as an unknown package, instead of a wire
	// error from a raw disk read.
	text, ok := cp.FileText(path)
	if !ok {
		return []protocol.FoldingRange{}, nil
	}
	frs, err := langfeat.FoldingRanges(cp, path)
	if err != nil {
		s.logger.Printf("server: folding ranges %s: %v", path, err)
		return []protocol.FoldingRange{}, nil
	}
	out := make([]protocol.FoldingRange, 0, len(frs))
	for _, fr := range frs {
		rng, ok := offsetRangeToLSP(text, fr.Range.StartOffset, fr.Range.EndOffset)
		if !ok {
			continue
		}
		out = append(out, protocol.FoldingRange{StartLine: rng.Start.Line, EndLine: rng.End.Line, Kind: foldingRangeKind(fr.Kind)})
	}
	return out, nil
}

func foldingRangeKind(k langfeat.FoldingKind) protocol.FoldingRangeKind {
	switch k {
	case langfeat.FoldComment:
		return protocol.FoldingRangeKindComment
	case langfeat.FoldImports:
		return protocol.FoldingRangeKindImports
	default:
		return ""
	}
}

// handleSelectionRange answers textDocument/selectionRange.
func (s *Server) handleSelectionRange(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.SelectionRangeParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	ws := s.workspace()
	if ws == nil {
		return []protocol.SelectionRange{}, nil
	}
	cp, err := s.resolveCheckedPackage(ctx, ws, path)
	if err != nil {
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return []protocol.SelectionRange{}, nil
	}
	// FileText, not a separate s.overlay.ReadFile(path): see the identical
	// comment in handleFoldingRange.
	text, ok := cp.FileText(path)
	if !ok {
		return []protocol.SelectionRange{}, nil
	}
	out := make([]protocol.SelectionRange, len(p.Positions))
	for i, pos := range p.Positions {
		out[i] = s.selectionRangeAt(cp, path, text, pos)
	}
	return out, nil
}

// selectionRangeAt builds the innermost-to-outermost SelectionRange chain
// for pos, falling back to a zero-width range at pos if nothing resolves
// (the LSP response must have one entry per requested position).
func (s *Server) selectionRangeAt(cp *check.CheckedPackage, path string, text []byte, pos protocol.Position) protocol.SelectionRange {
	fallback := protocol.SelectionRange{Range: protocol.Range{Start: pos, End: pos}}
	offset, ok := byteOffsetForPosition(text, pos)
	if !ok {
		return fallback
	}
	ranges, err := langfeat.SelectionRanges(cp, path, offset)
	if err != nil {
		s.logger.Printf("server: selection ranges %s: %v", path, err)
		return fallback
	}
	if len(ranges) == 0 {
		return fallback
	}
	var node *protocol.SelectionRange
	for i := len(ranges) - 1; i >= 0; i-- {
		rng, ok := offsetRangeToLSP(text, ranges[i].StartOffset, ranges[i].EndOffset)
		if !ok {
			s.logger.Printf("server: selection range %s: range [%d,%d) outside current buffer", path, ranges[i].StartOffset, ranges[i].EndOffset)
			continue
		}
		node = &protocol.SelectionRange{Range: rng, Parent: node}
	}
	if node == nil {
		return fallback
	}
	return *node
}

// handleDocumentRangeFormatting answers textDocument/rangeFormatting: gofmt
// applied to the whole file, with only the hunk of the resulting diff that
// overlaps the requested range returned as an edit.
func (s *Server) handleDocumentRangeFormatting(_ context.Context, params json.RawMessage) (any, error) {
	var p protocol.DocumentRangeFormattingParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return nil, err
	}
	formatted, err := langfeat.Format(text)
	if err != nil {
		// A file with syntax errors cannot be formatted; report no edits
		// rather than failing the request (mirrors handleFormatting).
		s.logger.Printf("server: range format %s: %v", path, err)
		return []protocol.TextEdit{}, nil
	}
	edits := rangeFormatEdits(text, formatted, p.Range)
	if edits == nil {
		return []protocol.TextEdit{}, nil
	}
	return edits, nil
}

// rangeFormatEdits computes gofmt's change to text as the minimal set of
// line-granularity edits (see internal/diff, which computes them via Myers'
// shortest-edit-script algorithm on the already-gofmt'd text), then keeps
// only the edits whose range overlaps rng. Each returned edit's NewText is
// an exact substring of formatted, so it is what gofmt already produced for
// that region in place — never a fragment reformatted in isolation, which
// could lose indentation context and no longer match gofmt's own output.
// This also means a hunk for an unrelated, independently-misformatted
// region elsewhere in the file is never returned alongside one that
// overlaps rng, matching gopls's per-hunk-confined
// textDocument/rangeFormatting. Returns nil if text is already formatted or
// no hunk overlaps rng.
func rangeFormatEdits(text, formatted []byte, rng protocol.Range) []protocol.TextEdit {
	var out []protocol.TextEdit
	for _, e := range diff.Lines(text, formatted) {
		startPos, ok1 := overlay.UTF16PositionForByteOffset(text, e.Start)
		endPos, ok2 := overlay.UTF16PositionForByteOffset(text, e.End)
		if !ok1 || !ok2 {
			continue
		}
		editRange := protocol.Range{Start: startPos, End: endPos}
		if !rangesOverlap(editRange, rng) {
			continue
		}
		out = append(out, protocol.TextEdit{Range: editRange, NewText: e.New})
	}
	return out
}

func rangesOverlap(a, b protocol.Range) bool {
	return !positionBefore(a.End, b.Start) && !positionBefore(b.End, a.Start)
}

func positionBefore(a, b protocol.Position) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Character < b.Character
}

// handleDocumentLink answers textDocument/documentLink: each import spec
// links to either a local workspace file or a pkg.go.dev page.
func (s *Server) handleDocumentLink(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.DocumentLinkParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	ws := s.workspace()
	if ws == nil {
		return []protocol.DocumentLink{}, nil
	}
	cp, err := s.resolveCheckedPackage(ctx, ws, path)
	if err != nil {
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return []protocol.DocumentLink{}, nil
	}
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return nil, err
	}
	links, err := langfeat.ImportLinks(cp, path)
	if err != nil {
		s.logger.Printf("server: document links %s: %v", path, err)
		return []protocol.DocumentLink{}, nil
	}
	out := make([]protocol.DocumentLink, 0, len(links))
	for _, l := range links {
		rng, ok := offsetRangeToLSP(text, l.Range.StartOffset, l.Range.EndOffset)
		if !ok {
			s.logger.Printf("server: document link %s: range [%d,%d) outside current buffer", path, l.Range.StartOffset, l.Range.EndOffset)
			continue
		}
		target, ok := documentLinkTarget(ws, l.PkgPath)
		if !ok {
			s.logger.Printf("server: document link %s: no link target for import %q", path, l.PkgPath)
			continue
		}
		out = append(out, protocol.DocumentLink{Range: rng, Target: &target})
	}
	return out, nil
}

// documentLinkTarget resolves pkgPath to a link target: a local workspace
// file (its first Go file) if pkgPath's directory is inside ws.root,
// otherwise its pkg.go.dev page.
func documentLinkTarget(ws *workspace, pkgPath string) (uri.URI, bool) {
	pkg, ok := ws.snap.Package(pkgPath)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(pkg.Dir, ws.root) {
		if len(pkg.GoFiles) == 0 {
			return "", false
		}
		return uri.File(pkg.GoFiles[0]), true
	}
	return uri.URI("https://pkg.go.dev/" + pkgPath), true
}

// handleCompletionWithData wraps handleCompletion, embedding a
// langfeat.CompletionDocKey in each item's Data field so a later
// completionItem/resolve request can fill in Documentation without every
// completion response having to resolve it eagerly.
func (s *Server) handleCompletionWithData(ctx context.Context, params json.RawMessage) (any, error) {
	result, err := s.handleCompletion(ctx, params)
	if err != nil {
		return result, err
	}
	items, ok := result.(protocol.CompletionItemSlice)
	if !ok || len(items) == 0 {
		return result, nil
	}
	var p protocol.CompletionParams
	if uerr := protocol.Unmarshal(params, &p); uerr != nil {
		s.logger.Printf("server: completion data params: %v", uerr)
		return result, nil
	}
	cf := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if !cf.ok {
		return result, nil
	}
	for i := range items {
		key := langfeat.CompletionDocKey{File: cf.path, Offset: cf.offset, Label: items[i].Label}
		b, merr := json.Marshal(key)
		if merr != nil {
			continue
		}
		items[i].Data = protocol.LSPAny(b)
	}
	return items, nil
}

// handleCompletionResolve answers completionItem/resolve: fills in
// Documentation for an item previously returned by handleCompletionWithData.
func (s *Server) handleCompletionResolve(ctx context.Context, params json.RawMessage) (any, error) {
	var item protocol.CompletionItem
	if err := protocol.Unmarshal(params, &item); err != nil {
		return nil, err
	}
	if len(item.Data) == 0 {
		return &item, nil
	}
	var key langfeat.CompletionDocKey
	if err := json.Unmarshal(item.Data, &key); err != nil {
		s.logger.Printf("server: completion resolve data: %v", err)
		return &item, nil
	}
	ws := s.workspace()
	if ws == nil {
		return &item, nil
	}
	cp, err := s.resolveCheckedPackage(ctx, ws, key.File)
	if err != nil {
		s.logger.Printf("server: completion resolve %s: %v", key.File, err)
		return &item, nil
	}
	info, err := langfeat.ResolveCompletionDoc(cp, s.overlay, key)
	if err != nil {
		s.logger.Printf("server: completion resolve doc %s: %v", key.File, err)
		return &item, nil
	}
	if info == nil {
		return &item, nil
	}
	var doc string
	if info.UnimportedSelector != "" {
		doc = s.unimportedCompletionDoc(ctx, ws, cp.PkgPath(), info.UnimportedSelector, key.Label)
	} else {
		doc = s.completionDoc(ctx, info)
	}
	if doc != "" {
		item.Documentation = protocol.String(doc)
	}
	return &item, nil
}

// completionDoc resolves info's doc comment: info.Doc directly if it is
// already the answer (a same-package candidate), otherwise crossPackageDoc
// for a cross-package one (a workspace facts-index lookup for a root
// package, internal/depcheck for a standard library/module dependency —
// see its doc; handleHover shares the identical fallback for the same
// reason).
func (s *Server) completionDoc(ctx context.Context, info *langfeat.CompletionDocInfo) string {
	if info.Doc != "" {
		return info.Doc
	}
	if info.PkgPath == "" {
		return ""
	}
	return s.crossPackageDoc(ctx, info.PkgPath, info.ObjPath)
}

// unimportedCompletionDoc resolves the doc comment for label, an exported
// member of whichever graph-known package named selector the completionItem/
// resolve request's original candidate actually came from (see
// langfeat.CompletionDocInfo.UnimportedSelector's doc): ResolveCompletionDoc
// itself has no graph access to redo appendUnimportedCompletions's own
// package-name-to-import-path lookup, so this repeats it here, trying each
// candidate package sharing selector's declared name (ws.depProvider parses
// from source, the same doc-capable path crossPackageDoc already uses for
// an already-imported dependency) and stopping at the first whose scope
// actually declares label.
func (s *Server) unimportedCompletionDoc(ctx context.Context, ws *workspace, ownPath, selector, label string) string {
	var errs []string
	for _, path := range ws.pkgNameIndex[selector] {
		if path == ownPath {
			continue
		}
		candidate, err := ws.depProvider.Package(ctx, path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		obj := candidate.Types().Scope().Lookup(label)
		if obj == nil || !obj.Exported() {
			continue
		}
		objPath, err := objectpath.For(depcheck.OriginObject(obj))
		if err != nil {
			continue
		}
		if doc := s.crossPackageDoc(ctx, path, string(objPath)); doc != "" {
			return doc
		}
	}
	// Logged only once every same-named candidate has been tried and none
	// produced a result, mirroring unimportedMemberItems's identical
	// precedent (handlers_completion_unimported.go): several packages
	// sharing selector's declared name, with only one actually usable here,
	// is the ordinary case and stays silent.
	if len(errs) > 0 {
		s.logger.Printf("server: unimported completion doc for %s.%s: %d candidate package(s) failed to parse: %s",
			selector, label, len(errs), strings.Join(errs, "; "))
	}
	return ""
}
