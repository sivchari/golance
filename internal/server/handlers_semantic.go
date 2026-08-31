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
// Tokens (sorted, byte-offset ranges) and its current buffer content. It
// returns (nil, nil, nil) — not an error — before the "initialize" request
// has populated the workspace, and likewise (logged, not surfaced) when
// path is not part of a known package or langfeat.SemanticTokens finds
// nothing to classify, matching every other handler's read-only convention
// for those states. err is non-nil only for a genuine overlay-read
// failure.
func (s *Server) semanticTokensForFile(ctx context.Context, u uri.URI) ([]langfeat.Token, []byte, error) {
	path := u.FsPath()
	ws := s.workspace()
	if ws == nil {
		return nil, nil, nil
	}
	cp, err := ws.engine.Get(ctx, path)
	if err != nil {
		s.logger.Printf("server: checked package for %s: %v", path, err)
		return nil, nil, nil
	}
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	toks, err := langfeat.SemanticTokens(cp, path, text)
	if err != nil {
		s.logger.Printf("server: semantic tokens %s: %v", path, err)
		return nil, nil, nil
	}
	return toks, text, nil
}

func (s *Server) handleSemanticTokensFull(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.SemanticTokensParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	toks, text, err := s.semanticTokensForFile(ctx, p.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return &protocol.SemanticTokens{Data: langfeat.Encode(text, toks)}, nil
}

func (s *Server) handleSemanticTokensRange(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.SemanticTokensRangeParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	toks, text, err := s.semanticTokensForFile(ctx, p.TextDocument.URI)
	if err != nil {
		return nil, err
	}
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
