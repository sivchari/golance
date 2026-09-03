package golance_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// initializeWithCodeLenses performs the LSP handshake like
// lspClient.initialize, with an explicit "codelenses" initializationOption
// (nil sends none, i.e. golance's own defaults — generate/regenerate_cgo
// on, test off, matching gopls). Copied from e2e_inlayhint_test.go's own
// initializeWithHints for the identical reason that file's doc gives: kept
// in its own file rather than touching e2e_client_test.go while other work
// may be in flight there.
func initializeWithCodeLenses(t *testing.T, c *lspClient, root string, codelenses map[string]bool) {
	t.Helper()
	gopid := os.Getpid()
	if gopid < 0 || gopid > math.MaxInt32 {
		t.Fatalf("pid %d does not fit in int32", gopid)
		return
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
	if codelenses != nil {
		optsJSON, err := json.Marshal(struct {
			Codelenses map[string]bool `json:"codelenses"`
		}{Codelenses: codelenses})
		if err != nil {
			t.Fatalf("marshal codelenses initializationOptions: %v", err)
		}
		params.InitializationOptions = protocol.LSPAny(optsJSON)
	}
	resp := c.call(t, protocol.MethodInitialize, params, e2eIndexBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("initialize failed: %s", resp.Error)
	}
	var initResult protocol.InitializeResult
	if err := protocol.Unmarshal(resp.Result, &initResult); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if initResult.Capabilities.CodeLensProvider == nil {
		t.Error("InitializeResult.Capabilities.CodeLensProvider = nil, want non-nil")
	}
	c.notify(t, protocol.MethodInitialized, &protocol.InitializedParams{})
}

// requestCodeLensE2E sends textDocument/codeLens for path and returns its
// result.
func requestCodeLensE2E(t *testing.T, c *lspClient, path string) []protocol.CodeLens {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentCodeLens, &protocol.CodeLensParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("codeLens failed: %s", resp.Error)
	}
	var lenses []protocol.CodeLens
	if err := protocol.Unmarshal(resp.Result, &lenses); err != nil {
		t.Fatalf("unmarshal codeLens result: %v", err)
	}
	return lenses
}

// findCodeLensByTitle returns the entry of lenses whose Command.Title is
// title.
func findCodeLensByTitle(lenses []protocol.CodeLens, title string) (protocol.CodeLens, bool) {
	for i := range lenses {
		if lenses[i].Command.Title == title {
			return lenses[i], true
		}
	}
	return protocol.CodeLens{}, false
}

// TestE2E_CodeLens_Generate drives a real golance binary over stdio and
// verifies textDocument/codeLens's default-on go:generate source: a plain
// .go file with a "//go:generate" directive gets both a recursive and a
// non-recursive "run go generate" lens with no special initialization
// option (golance's own default matches gopls's own default-on
// generate/regenerate_cgo sources — see internal/server/
// handlers_codelens.go).
func TestE2E_CodeLens_Generate(t *testing.T) {
	skipUnlessE2E(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	writeE2EFile(t, root, "go.mod", "module example.com/e2ecodelens\n\ngo 1.23\n")
	const src = `package gen

//go:generate stringer -type=Kind

type Kind int
`
	genFile := writeE2EFile(t, root, "gen/gen.go", src)

	c := startClient(t, root)
	initializeWithCodeLenses(t, c, root, nil)
	c.openFile(t, genFile)

	lenses := requestCodeLensE2E(t, c, genFile)
	if len(lenses) != 2 {
		t.Fatalf("codeLens = %+v, want exactly 2 (recursive + non-recursive go generate)", lenses)
	}
	if _, ok := findCodeLensByTitle(lenses, "run go generate ./..."); !ok {
		t.Errorf("codeLens = %+v, want a %q entry", lenses, "run go generate ./...")
	}
	if _, ok := findCodeLensByTitle(lenses, "run go generate"); !ok {
		t.Errorf("codeLens = %+v, want a %q entry", lenses, "run go generate")
	}
}

// TestE2E_CodeLens_RunTest drives a real golance binary over stdio and
// exercises the full textDocument/codeLens -> workspace/executeCommand
// round trip for a Test function: with the "test" code lens source enabled
// via initializationOptions (off by default, matching gopls — see
// internal/server/handlers_codelens.go), a _test.go file's TestAdd gets a
// "run test" lens whose Command golance actually executes as `go test
// -run=^TestAdd$` when dispatched through workspace/executeCommand.
func TestE2E_CodeLens_RunTest(t *testing.T) {
	skipUnlessE2E(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	writeE2EFile(t, root, "go.mod", "module example.com/e2ecodelensrun\n\ngo 1.23\n")
	writeE2EFile(t, root, "run/run.go", "package run\n\n// Add returns a+b.\nfunc Add(a, b int) int { return a + b }\n")
	const testSrc = `package run

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("wrong sum")
	}
}
`
	testFile := writeE2EFile(t, root, "run/run_test.go", testSrc)

	c := startClient(t, root)
	initializeWithCodeLenses(t, c, root, map[string]bool{"test": true})
	c.openFile(t, testFile)

	lenses := requestCodeLensE2E(t, c, testFile)
	lens, ok := findCodeLensByTitle(lenses, "run test")
	if !ok {
		t.Fatalf("codeLens = %+v, want a %q entry", lenses, "run test")
	}
	if lens.Command.Command == "" {
		t.Fatalf("run test lens has no Command.Command: %+v", lens)
	}

	execResp := c.call(t, protocol.MethodWorkspaceExecuteCommand, &protocol.ExecuteCommandParams{
		Command:   lens.Command.Command,
		Arguments: lens.Command.Arguments,
	}, e2eIndexBudget)
	if len(execResp.Error) > 0 {
		t.Fatalf("workspace/executeCommand(%s) failed: %s", lens.Command.Command, execResp.Error)
	}
}
