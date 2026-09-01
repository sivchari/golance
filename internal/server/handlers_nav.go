package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/rpc"
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
	return s.typeDefinitionCrossPackage(ctx, info)
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
// the on-disk facts index.
func (s *Server) typeDefinitionCrossPackage(ctx context.Context, info *langfeat.TypeDefInfo) (any, error) {
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	loc, ok := resolver.TypeDeclaration(ctx, info.PkgPath, info.ObjPath)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	pl, ok := s.correctResultLocation(loc)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	return protocol.LocationSlice{pl}, nil
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
	cp, err := ws.engine.Get(ctx, path)
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
	cp, err := ws.engine.Get(ctx, path)
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
		out[i] = selectionRangeAt(cp, path, text, pos)
	}
	return out, nil
}

// selectionRangeAt builds the innermost-to-outermost SelectionRange chain
// for pos, falling back to a zero-width range at pos if nothing resolves
// (the LSP response must have one entry per requested position).
func selectionRangeAt(cp *check.CheckedPackage, path string, text []byte, pos protocol.Position) protocol.SelectionRange {
	fallback := protocol.SelectionRange{Range: protocol.Range{Start: pos, End: pos}}
	offset, ok := byteOffsetForPosition(text, pos)
	if !ok {
		return fallback
	}
	ranges, err := langfeat.SelectionRanges(cp, path, offset)
	if err != nil || len(ranges) == 0 {
		return fallback
	}
	var node *protocol.SelectionRange
	for i := len(ranges) - 1; i >= 0; i-- {
		rng, ok := offsetRangeToLSP(text, ranges[i].StartOffset, ranges[i].EndOffset)
		if !ok {
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
	edit, ok := rangeFormatEdit(text, formatted, p.Range)
	if !ok {
		return []protocol.TextEdit{}, nil
	}
	return []protocol.TextEdit{edit}, nil
}

// rangeFormatEdit computes gofmt's whole-file change to text as a single
// edit confined to the smallest contiguous span of changed lines (the
// common-prefix/common-suffix line diff between text and formatted). ok is
// false if the file is already formatted, or the changed span does not
// overlap rng at all.
func rangeFormatEdit(text, formatted []byte, rng protocol.Range) (protocol.TextEdit, bool) {
	if bytes.Equal(text, formatted) {
		return protocol.TextEdit{}, false
	}
	fromLines := bytes.Split(text, []byte{'\n'})
	toLines := bytes.Split(formatted, []byte{'\n'})
	prefix, suffix := commonPrefixSuffix(fromLines, toLines)

	fromOffs := lineOffsets(fromLines)
	toOffs := lineOffsets(toLines)
	changeStart := fromOffs[prefix]
	changeEndFrom := fromOffs[len(fromLines)-suffix]
	changeEndTo := toOffs[len(toLines)-suffix]

	startPos, ok1 := overlay.UTF16PositionForByteOffset(text, changeStart)
	endPos, ok2 := overlay.UTF16PositionForByteOffset(text, changeEndFrom)
	if !ok1 || !ok2 {
		return protocol.TextEdit{}, false
	}
	editRange := protocol.Range{Start: startPos, End: endPos}
	if !rangesOverlap(editRange, rng) {
		return protocol.TextEdit{}, false
	}
	return protocol.TextEdit{Range: editRange, NewText: string(formatted[toOffs[prefix]:changeEndTo])}, true
}

// commonPrefixSuffix returns the number of leading and (non-overlapping)
// trailing lines a and b have in common.
func commonPrefixSuffix(a, b [][]byte) (prefix, suffix int) {
	n := min(len(a), len(b))
	for prefix < n && bytes.Equal(a[prefix], b[prefix]) {
		prefix++
	}
	maxSuffix := n - prefix
	for suffix < maxSuffix && bytes.Equal(a[len(a)-1-suffix], b[len(b)-1-suffix]) {
		suffix++
	}
	return prefix, suffix
}

// lineOffsets returns, for each index in [0, len(lines)], the byte offset
// of the start of lines[idx] in bytes.Join(lines, "\n") — lineOffsets(lines)
// [len(lines)] is that joined content's total length.
func lineOffsets(lines [][]byte) []int {
	offs := make([]int, len(lines)+1)
	pos := 0
	for i, l := range lines {
		offs[i] = pos
		pos += len(l)
		if i < len(lines)-1 {
			pos++ // the '\n' separating this line from the next
		}
	}
	offs[len(lines)] = pos
	return offs
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
	cp, err := ws.engine.Get(ctx, path)
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
			continue
		}
		target, ok := documentLinkTarget(ws, l.PkgPath)
		if !ok {
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
	cp, err := ws.engine.Get(ctx, key.File)
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
	if doc := s.completionDoc(ctx, info); doc != "" {
		item.Documentation = protocol.String(doc)
	}
	return &item, nil
}

// completionDoc resolves info's doc comment: info.Doc directly if it is
// already the answer (a same-package candidate), otherwise a facts-index
// lookup for a cross-package one.
func (s *Server) completionDoc(ctx context.Context, info *langfeat.CompletionDocInfo) string {
	if info.Doc != "" {
		return info.Doc
	}
	if info.PkgPath == "" {
		return ""
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return ""
	}
	doc, _ := resolver.SymbolDoc(ctx, info.PkgPath, info.ObjPath)
	return doc
}
