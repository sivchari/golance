package golance_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestE2EInlayHint drives a real golance binary over stdio and verifies
// textDocument/inlayHint end to end: the default session (no "hints"
// initializationOption) returns both an assignVariableTypes and a
// parameterNames hint and honors a narrowed Range, and a session that
// disables parameterNames via initializationOptions gets none.
//
// This lives in its own file rather than adding to e2e_test.go /
// e2e_repo_test.go to avoid touching them while other work is in flight
// there; initializeWithHints below is this file's own copy of
// lspClient.initialize with an explicit "hints" option for the same
// reason.
func TestE2EInlayHint(t *testing.T) {
	skipUnlessE2E(t)

	root, _ := writeE2EModule(t)
	const inlaySrc = `package inlaydemo

func addNamed(x, y int) int { return x + y }

// Demo calls addNamed and assigns its result.
func Demo() int {
	total := addNamed(1, 2)
	return total
}
`
	inlayFile := writeE2EFile(t, root, "inlaydemo/inlaydemo.go", inlaySrc)
	fullRange := protocol.Range{End: endOfDocument(inlaySrc)}

	t.Run("default_enables_every_kind", func(t *testing.T) {
		c := startClient(t, root)
		initializeWithHints(t, c, root, nil)
		c.openFile(t, inlayFile)

		hints := requestInlayHints(t, c, inlayFile, fullRange)
		if !hasInlayHintLabel(hints, ": int") {
			t.Errorf("inlay hints = %+v, want an assignVariableTypes %q hint", hints, ": int")
		}
		if !hasInlayHintLabel(hints, "x:") || !hasInlayHintLabel(hints, "y:") {
			t.Errorf("inlay hints = %+v, want parameterNames %q and %q hints", hints, "x:", "y:")
		}
	})

	t.Run("range_excludes_hints_outside_it", func(t *testing.T) {
		c := startClient(t, root)
		initializeWithHints(t, c, root, nil)
		c.openFile(t, inlayFile)

		// A range ending right after "func addNamed(...) ..." excludes
		// Demo's body entirely, so neither the assignVariableTypes nor the
		// parameterNames hint should appear.
		narrow := protocol.Range{End: protocol.Position{Line: 3, Character: 0}}
		hints := requestInlayHints(t, c, inlayFile, narrow)
		if hasInlayHintLabel(hints, ": int") || hasInlayHintLabel(hints, "x:") {
			t.Errorf("inlay hints in narrowed range = %+v, want none of Demo's hints", hints)
		}
	})

	t.Run("hints_option_disables_parameterNames", func(t *testing.T) {
		c := startClient(t, root)
		initializeWithHints(t, c, root, map[string]bool{"parameterNames": false})
		c.openFile(t, inlayFile)

		hints := requestInlayHints(t, c, inlayFile, fullRange)
		if hasInlayHintLabel(hints, "x:") || hasInlayHintLabel(hints, "y:") {
			t.Errorf("inlay hints = %+v, want no parameterNames hints (disabled)", hints)
		}
		if !hasInlayHintLabel(hints, ": int") {
			t.Errorf("inlay hints = %+v, want assignVariableTypes still enabled", hints)
		}
	})
}

// initializeWithHints performs the LSP handshake like lspClient.initialize,
// with an explicit "hints" initializationOption (nil sends none, i.e.
// golance's default of every kind enabled).
func initializeWithHints(t *testing.T, c *lspClient, root string, hints map[string]bool) {
	t.Helper()
	gopid := os.Getpid()
	if gopid < 0 || gopid > math.MaxInt32 {
		t.Fatalf("pid %d does not fit in int32", gopid)
	}
	pid := int32(gopid)
	params := &protocol.InitializeParams{
		ProcessID: &pid,
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{
				{URI: uri.File(root), Name: filepath.Base(root)},
			}),
		},
	}
	if hints != nil {
		optsJSON, err := json.Marshal(struct {
			Hints map[string]bool `json:"hints"`
		}{Hints: hints})
		if err != nil {
			t.Fatalf("marshal hints initializationOptions: %v", err)
		}
		params.InitializationOptions = protocol.LSPAny(optsJSON)
	}
	resp := c.call(t, protocol.MethodInitialize, params, e2eIndexBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("initialize failed: %s", resp.Error)
	}
	c.notify(t, protocol.MethodInitialized, &protocol.InitializedParams{})
}

// requestInlayHints sends textDocument/inlayHint for path over rng.
func requestInlayHints(t *testing.T, c *lspClient, path string, rng protocol.Range) []protocol.InlayHint {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentInlayHint, &protocol.InlayHintParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
		Range:        rng,
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("inlayHint failed: %s", resp.Error)
	}
	var hints []protocol.InlayHint
	if err := protocol.Unmarshal(resp.Result, &hints); err != nil {
		t.Fatalf("unmarshal inlayHint result: %v", err)
	}
	return hints
}

// hasInlayHintLabel reports whether any of hints has label as its (plain
// string) Label.
func hasInlayHintLabel(hints []protocol.InlayHint, label string) bool {
	for _, h := range hints {
		if s, ok := h.Label.(protocol.String); ok && string(s) == label {
			return true
		}
	}
	return false
}

// endOfDocument returns src's end position, in the 0-based line/character
// coordinates textDocument/inlayHint's Range.End takes: a Range whose end
// is past the document's actual last line fails to resolve to a byte
// offset (see internal/server.byteOffsetForPosition), so this is what a
// "whole document" query needs to send instead of an arbitrarily large
// sentinel.
func endOfDocument(src string) protocol.Position {
	lines := strings.Split(src, "\n")
	last := len(lines) - 1
	return protocol.Position{Line: uint32(last), Character: uint32(len(lines[last]))}
}
