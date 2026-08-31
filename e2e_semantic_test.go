package golance_test

import (
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestE2E_SemanticTokens drives a real golance binary over stdio and
// exercises textDocument/semanticTokens/full and .../range against a
// synthetic module, independently of TestE2E's shared session (see
// e2e_test.go) so it stays self-contained in its own file.
func TestE2E_SemanticTokens(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EModule(t)
	c := startClient(t, root)
	result := c.initialize(t, root)

	if result.Capabilities.SemanticTokensProvider == nil {
		t.Fatal("SemanticTokensProvider capability missing from InitializeResult")
	}

	c.openFile(t, locs.utilFile)

	full := semanticTokensFull(t, c, locs.utilFile)
	if len(full.Data) == 0 {
		t.Fatal("semanticTokens/full returned no data for a non-empty Go file")
	}
	if len(full.Data)%5 != 0 {
		t.Fatalf("semanticTokens/full data length = %d, want a multiple of 5", len(full.Data))
	}

	// locs.utilSrc is "package util\n\n// Sum adds two ints.\nfunc Sum(a, b int) int {\n\treturn a + b\n}\n":
	// restricting the range to line 0 only ("package util") should return
	// strictly fewer tokens than the full file.
	narrow := semanticTokensRange(t, c, locs.utilFile, protocol.Range{
		Start: protocol.Position{Line: 0, Character: 0},
		End:   protocol.Position{Line: 1, Character: 0},
	})
	if len(narrow.Data)%5 != 0 {
		t.Fatalf("semanticTokens/range data length = %d, want a multiple of 5", len(narrow.Data))
	}
	if len(narrow.Data) >= len(full.Data) {
		t.Errorf("semanticTokens/range (line 0 only) returned %d uint32s, want fewer than the full response's %d", len(narrow.Data), len(full.Data))
	}
}

func semanticTokensFull(t *testing.T, c *lspClient, path string) protocol.SemanticTokens {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentSemanticTokensFull, &protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("semanticTokens/full failed: %s", resp.Error)
	}
	var toks protocol.SemanticTokens
	if err := protocol.Unmarshal(resp.Result, &toks); err != nil {
		t.Fatalf("unmarshal semanticTokens/full result: %v", err)
	}
	return toks
}

func semanticTokensRange(t *testing.T, c *lspClient, path string, rng protocol.Range) protocol.SemanticTokens {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentSemanticTokensRange, &protocol.SemanticTokensRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
		Range:        rng,
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("semanticTokens/range failed: %s", resp.Error)
	}
	var toks protocol.SemanticTokens
	if err := protocol.Unmarshal(resp.Result, &toks); err != nil {
		t.Fatalf("unmarshal semanticTokens/range result: %v", err)
	}
	return toks
}
