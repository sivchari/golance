package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// checkedFileResult is checkedFile's result: the package's CheckedPackage,
// the document's current buffer content, and the query's byte offset into
// it. Ok is false when the workspace is not loaded yet or pos does not
// resolve to a valid offset in Text — both are "no result", not request
// failures.
type checkedFileResult struct {
	cp     *check.CheckedPackage
	path   string
	text   []byte
	offset int
	ok     bool
}

// checkedFile resolves an LSP TextDocumentPositionParams-style request to a
// checkedFileResult. err is non-nil only for a hard failure such as a read
// or type-check error.
func (s *Server) checkedFile(ctx context.Context, u uri.URI, pos protocol.Position) (checkedFileResult, error) {
	path := u.FsPath()
	ws := s.workspace()
	if ws == nil {
		return checkedFileResult{path: path}, nil
	}
	cp, err := ws.engine.Get(ctx, path)
	if err != nil {
		return checkedFileResult{path: path}, err
	}
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return checkedFileResult{path: path}, err
	}
	offset, ok := byteOffsetForPosition(text, pos)
	return checkedFileResult{cp: cp, path: path, text: text, offset: offset, ok: ok}, nil
}

func (s *Server) handleHover(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.HoverParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf, err := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if err != nil || !cf.ok {
		return nil, err
	}
	info, err := langfeat.Hover(cf.cp, cf.path, cf.offset)
	if err != nil || info == nil {
		return nil, err
	}
	rng, ok := offsetRangeToLSP(cf.text, info.Range.StartOffset, info.Range.EndOffset)
	if !ok {
		return nil, nil
	}
	return &protocol.Hover{
		Contents: &protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: hoverMarkdown(info)},
		Range:    &rng,
	}, nil
}

func hoverMarkdown(info *langfeat.HoverInfo) string {
	md := "```go\n" + info.Signature + "\n```"
	if info.Doc != "" {
		md += "\n\n" + info.Doc
	}
	return md
}

func (s *Server) handleCompletion(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.CompletionParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf, err := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if err != nil || !cf.ok {
		return protocol.CompletionItemSlice(nil), err
	}
	items, err := langfeat.Completion(cf.cp, s.overlay, cf.path, cf.offset)
	if err != nil {
		return nil, err
	}
	out := make(protocol.CompletionItemSlice, len(items))
	for i, it := range items {
		out[i] = protocol.CompletionItem{
			Label:    it.Label,
			Kind:     completionItemKind(it.Kind),
			Detail:   protocol.NewOptional(it.Detail),
			SortText: protocol.NewOptional(it.SortText),
		}
	}
	return out, nil
}

func (s *Server) handleSignatureHelp(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.SignatureHelpParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cf, err := s.checkedFile(ctx, p.TextDocument.URI, p.Position)
	if err != nil || !cf.ok {
		return nil, err
	}
	info, err := langfeat.SignatureHelp(cf.cp, cf.path, cf.offset)
	if err != nil || info == nil {
		return nil, err
	}
	sigParams := make([]protocol.ParameterInformation, len(info.Params))
	for i, ps := range info.Params {
		sigParams[i] = protocol.ParameterInformation{Label: protocol.String(ps)}
	}
	activeParam := info.ActiveParam
	if activeParam < 0 {
		activeParam = 0
	}
	if activeParam > math.MaxUint32 {
		activeParam = math.MaxUint32
	}
	return &protocol.SignatureHelp{
		Signatures: []protocol.SignatureInformation{{
			Label:           info.Label,
			Parameters:      sigParams,
			ActiveParameter: protocol.NewNullable(uint32(activeParam)),
		}},
	}, nil
}

func (s *Server) handleDocumentSymbol(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.DocumentSymbolParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	ws := s.workspace()
	if ws == nil {
		return protocol.DocumentSymbolSlice(nil), nil
	}
	cp, err := ws.engine.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return nil, err
	}
	syms, err := langfeat.DocumentSymbols(cp, path)
	if err != nil {
		return nil, err
	}
	out := make(protocol.DocumentSymbolSlice, 0, len(syms))
	for _, sym := range syms {
		if ds, ok := toDocumentSymbol(text, sym); ok {
			out = append(out, ds)
		}
	}
	return out, nil
}

func toDocumentSymbol(text []byte, sym langfeat.Symbol) (protocol.DocumentSymbol, bool) {
	rng, ok := offsetRangeToLSP(text, sym.Range.StartOffset, sym.Range.EndOffset)
	if !ok {
		return protocol.DocumentSymbol{}, false
	}
	children := make([]protocol.DocumentSymbol, 0, len(sym.Children))
	for _, c := range sym.Children {
		if cds, ok := toDocumentSymbol(text, c); ok {
			children = append(children, cds)
		}
	}
	return protocol.DocumentSymbol{
		Name:           sym.Name,
		Kind:           documentSymbolKind(sym.Kind),
		Range:          rng,
		SelectionRange: rng,
		Children:       children,
	}, true
}

func (s *Server) handleInlayHint(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.InlayHintParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	ws := s.workspace()
	if ws == nil {
		return []protocol.InlayHint{}, nil
	}
	cp, err := ws.engine.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hints, err := langfeat.InlayHints(cp, path)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.InlayHint, 0, len(hints))
	for _, h := range hints {
		pos, ok := overlay.UTF16PositionForByteOffset(text, h.Offset)
		if !ok {
			continue
		}
		out = append(out, protocol.InlayHint{Position: pos, Label: protocol.String(h.Label)})
	}
	return out, nil
}

func (s *Server) handleFormatting(_ context.Context, params json.RawMessage) (any, error) {
	var p protocol.DocumentFormattingParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return nil, err
	}
	formatted, err := langfeat.OrganizeImports(path, text)
	if err != nil {
		// A file with syntax errors cannot be formatted; report no edits
		// rather than failing the request. Logged (not just discarded) so
		// an unexpectedly persistent failure is still visible in server
		// logs rather than silently indistinguishable from "nothing to
		// format."
		s.logger.Printf("server: organize imports for %s: %v", path, err)
		return []protocol.TextEdit{}, nil
	}
	if bytes.Equal(text, formatted) {
		return []protocol.TextEdit{}, nil
	}
	end, ok := overlay.UTF16PositionForByteOffset(text, len(text))
	if !ok {
		return []protocol.TextEdit{}, nil
	}
	return []protocol.TextEdit{{
		Range:   protocol.Range{Start: protocol.Position{}, End: end},
		NewText: string(formatted),
	}}, nil
}
