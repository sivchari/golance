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
// the exact buffer content it was checked against, and the query's byte
// offset into it. Ok is false when the workspace is not loaded yet, path
// is not part of a known package (e.g. a testdata fixture, an external
// _test package file, or a brand-new unsaved file the graph hasn't picked
// up yet — see check.Engine.Get), or pos does not resolve to a valid
// offset in Text — all "no result", not request failures.
type checkedFileResult struct {
	cp     *check.CheckedPackage
	path   string
	text   []byte
	offset int
	ok     bool
}

// checkedFile resolves an LSP TextDocumentPositionParams-style request to a
// checkedFileResult. Text and Offset are derived from cp.FileText, the
// exact content ws.engine.Get type-checked against, rather than a separate
// later overlay read: a concurrent edit landing between the two could
// otherwise leave Offset computed against different content than what cp
// was built from. The error return is always nil; kept for symmetry with
// the handlers that call this and to absorb a future hard-failure path
// without a signature change. A "path is not part of a known package"
// error from ws.engine.Get is logged and reported as an ordinary !ok "no
// result" instead (the client already renders that as "nothing here,"
// matching how handleCompletionResolve treats the identical Engine.Get
// failure).
func (s *Server) checkedFile(ctx context.Context, u uri.URI, pos protocol.Position) (checkedFileResult, error) {
	path := u.FsPath()
	ws := s.workspace()
	if ws == nil {
		return checkedFileResult{path: path}, nil
	}
	cp, err := ws.engine.Get(ctx, path)
	if err != nil {
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return checkedFileResult{path: path}, nil
	}
	text, ok := cp.FileText(path)
	if !ok {
		return checkedFileResult{path: path}, nil
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
	if err != nil {
		s.logger.Printf("server: hover %s: %v", cf.path, err)
		return nil, nil
	}
	if info == nil {
		return nil, nil
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
	items, err := langfeat.Completion(cf.cp, cf.text, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: completion %s: %v", cf.path, err)
		return protocol.CompletionItemSlice(nil), nil
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
	if err != nil {
		s.logger.Printf("server: signature help %s: %v", cf.path, err)
		return nil, nil
	}
	if info == nil {
		return nil, nil
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
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return protocol.DocumentSymbolSlice(nil), nil
	}
	text, ok := cp.FileText(path)
	if !ok {
		return protocol.DocumentSymbolSlice(nil), nil
	}
	syms, err := langfeat.DocumentSymbols(cp, path)
	if err != nil {
		s.logger.Printf("server: document symbols %s: %v", path, err)
		return protocol.DocumentSymbolSlice(nil), nil
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

// setHintsEnabled records which inlay hint kinds are enabled for s, in
// s.hints. It is replaced wholesale (never mutated in place), so concurrent
// reads from hintsEnabled never race with a write.
func (s *Server) setHintsEnabled(enabled map[langfeat.HintKind]bool) {
	s.hints.Store(&enabled)
}

// hintsEnabled returns which inlay hint kinds are enabled for s. Every kind
// is enabled until setHintsEnabled has run (before "initialize" completes,
// and in tests that never call it), matching golance's default of every
// hint kind on unless a client's "hints" settings say otherwise.
func (s *Server) hintsEnabled() map[langfeat.HintKind]bool {
	if enabled := s.hints.Load(); enabled != nil {
		return *enabled
	}
	return langfeat.ResolveHints(nil)
}

// hintsSettings is the "hints" shape golance reads from
// initializationOptions: the same key and kind names as gopls's own
// "hints" setting (see langfeat.AllHintKinds), so an editor config already
// written for gopls enables the same inlay hint kinds here unmodified.
type hintsSettings struct {
	Hints map[string]bool `json:"hints"`
}

// parseHintsSettings resolves raw — an initializationOptions payload, or a
// workspace/didChangeConfiguration notification's Settings, both the same
// hintsSettings shape — into a complete enabled-kind set via
// langfeat.ResolveHints. Missing or malformed settings resolve to every kind
// enabled, golance's default; this is client-controlled input, so a
// malformed payload is treated as absent rather than as a request failure.
func parseHintsSettings(raw protocol.LSPAny) map[langfeat.HintKind]bool {
	if len(raw) == 0 {
		return langfeat.ResolveHints(nil)
	}
	var settings hintsSettings
	if err := protocol.Unmarshal(raw, &settings); err != nil {
		return langfeat.ResolveHints(nil)
	}
	return langfeat.ResolveHints(settings.Hints)
}

// handleDidChangeConfiguration answers workspace/didChangeConfiguration:
// re-resolves the enabled inlay hint kinds from params.Settings, which
// carries the same "hints" shape as initializationOptions (hintsSettings),
// so an editor's live settings change takes effect on the next
// textDocument/inlayHint request.
//
// It does not push workspace/inlayHint/refresh to make open files' inlay
// hints re-request themselves immediately: that is an outbound
// server-to-client request awaiting a response, and internal/rpc.Server
// currently only supports fire-and-forget server-to-client notifications
// (Notify), not request/response correlation in that direction. Adding
// that is a larger change to internal/rpc than this handler warrants: the
// client sees the update on its next inlay hint request regardless
// (e.g. on scroll or edit).
func (s *Server) handleDidChangeConfiguration(_ context.Context, params json.RawMessage) error {
	var p protocol.DidChangeConfigurationParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return err
	}
	s.setHintsEnabled(parseHintsSettings(p.Settings))
	return nil
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
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return []protocol.InlayHint{}, nil
	}
	text, ok := cp.FileText(path)
	if !ok {
		return []protocol.InlayHint{}, nil
	}
	start, ok := byteOffsetForPosition(text, p.Range.Start)
	if !ok {
		return []protocol.InlayHint{}, nil
	}
	end, ok := byteOffsetForPosition(text, p.Range.End)
	if !ok {
		return []protocol.InlayHint{}, nil
	}
	hints, err := langfeat.InlayHints(cp, path, start, end, s.hintsEnabled())
	if err != nil {
		s.logger.Printf("server: inlay hints %s: %v", path, err)
		return []protocol.InlayHint{}, nil
	}
	out := make([]protocol.InlayHint, 0, len(hints))
	for _, h := range hints {
		pos, ok := overlay.UTF16PositionForByteOffset(text, h.Offset)
		if !ok {
			continue
		}
		out = append(out, inlayHintToLSP(pos, h))
	}
	return out, nil
}

// inlayHintToLSP builds the protocol.InlayHint for h at pos: its render
// kind maps to protocol.InlayHintKind, and padding is set only where h
// requests it (an unset *bool omits the field, which a client treats the
// same as an explicit false).
func inlayHintToLSP(pos protocol.Position, h langfeat.Hint) protocol.InlayHint {
	hint := protocol.InlayHint{Position: pos, Label: protocol.String(h.Label)}
	switch h.Render {
	case langfeat.RenderType:
		hint.Kind = protocol.InlayHintKindType
	case langfeat.RenderParameter:
		hint.Kind = protocol.InlayHintKindParameter
	}
	if h.PaddingLeft {
		hint.PaddingLeft = boolPtr(true)
	}
	if h.PaddingRight {
		hint.PaddingRight = boolPtr(true)
	}
	return hint
}

func boolPtr(b bool) *bool { return &b }

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
