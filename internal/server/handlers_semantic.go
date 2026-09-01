package server

import (
	"context"
	"encoding/json"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/rpc"
)

// registerSemanticHandlers registers textDocument/semanticTokens/full and
// textDocument/semanticTokens/range on s.rpc. Both run on the Interactive
// pool: like hover and completion, each operates on a single already-open
// document, not a workspace-wide query.
func (s *Server) registerSemanticHandlers() {
	s.rpc.Handle(protocol.MethodTextDocumentSemanticTokensFull, rpc.Interactive, s.handleSemanticTokensFull)
	s.rpc.Handle(protocol.MethodTextDocumentSemanticTokensRange, rpc.Interactive, s.handleSemanticTokensRange)
}

// semanticTokensForFile resolves u's file to its classified semantic
// Tokens (sorted, byte-offset ranges) and the exact buffer content
// ws.engine.Get type-checked against — via cp.FileText, not a separate
// later overlay read, so a concurrent edit landing in between can't leave
// the two disagreeing (langfeat.SemanticTokens requires text to match cp
// exactly). It returns (nil, nil, nil) — not an error — before the
// "initialize" request has populated the workspace, and likewise (logged,
// not surfaced) when path is not part of a known package or
// langfeat.SemanticTokens finds nothing to classify, matching every other
// handler's read-only convention for those states.
func (s *Server) semanticTokensForFile(ctx context.Context, u uri.URI) ([]langfeat.Token, []byte) {
	path := u.FsPath()
	ws := s.workspace()
	if ws == nil {
		return nil, nil
	}
	cp, err := s.resolveCheckedPackage(ctx, ws, path)
	if err != nil {
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return nil, nil
	}
	text, ok := cp.FileText(path)
	if !ok {
		return nil, nil
	}
	toks, err := langfeat.SemanticTokens(cp, path, text)
	if err != nil {
		s.logger.Printf("server: semantic tokens %s: %v", path, err)
		return nil, nil
	}
	return toks, text
}

func (s *Server) handleSemanticTokensFull(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.SemanticTokensParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	toks, text := s.semanticTokensForFile(ctx, p.TextDocument.URI)
	return &protocol.SemanticTokens{Data: langfeat.Encode(text, toks)}, nil
}

func (s *Server) handleSemanticTokensRange(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.SemanticTokensRangeParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	toks, text := s.semanticTokensForFile(ctx, p.TextDocument.URI)
	return &protocol.SemanticTokens{Data: langfeat.Encode(text, tokensInRange(text, toks, p.Range))}, nil
}

// tokensInRange returns the subset of toks (sorted by Range.StartOffset)
// that intersects rng, converting rng's LSP (UTF-16) positions to byte
// offsets against text. A position that does not resolve (rng comes from
// a stale client view of the document) falls back to the corresponding
// end of the file, so the request degrades to "everything from/to here"
// rather than silently returning nothing.
func tokensInRange(text []byte, toks []langfeat.Token, rng protocol.Range) []langfeat.Token {
	start, ok := byteOffsetForPosition(text, rng.Start)
	if !ok {
		start = 0
	}
	end, ok := byteOffsetForPosition(text, rng.End)
	if !ok {
		end = len(text)
	}
	out := make([]langfeat.Token, 0, len(toks))
	for _, t := range toks {
		if t.Range.StartOffset < end && t.Range.EndOffset > start {
			out = append(out, t)
		}
	}
	return out
}
