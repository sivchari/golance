package golance_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eBuiltinNavLocs records the positions TestE2E_BuiltinNavigation
// queries, once for a workspace file and once for a _test.go file — golance
// must resolve a universe (predeclared) identifier's definition/hover
// identically regardless of which kind of file references it.
type e2eBuiltinNavLocs struct {
	appFile  string
	testFile string

	appNilPos, appErrorPos, appLenPos    protocol.Position
	testNilPos, testErrorPos, testLenPos protocol.Position
}

func writeE2EBuiltinNavModule(t *testing.T) (string, e2eBuiltinNavLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eBuiltinNavLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2ebuiltinnav\n\ngo 1.23\n")

	const appSrc = `package app

func Count(v []int) int {
	return len(v)
}

func IsNil(p *int) bool {
	return p == nil
}

func Describe(err error) string {
	return err.Error()
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)
	locs.appLenPos = mustPos(t, appSrc, "return len(v)", "len")
	locs.appNilPos = mustPos(t, appSrc, "p == nil", "nil")
	locs.appErrorPos = mustPos(t, appSrc, "func Describe(err error) string {", "error")

	const testSrc = `package app

func countFromTest(v []int) int {
	return len(v)
}

func isNilFromTest(p *int) bool {
	return p == nil
}

func describeFromTest(err error) string {
	return err.Error()
}
`
	locs.testFile = writeE2EFile(t, root, "app/app_test.go", testSrc)
	locs.testLenPos = mustPos(t, testSrc, "return len(v)", "len")
	locs.testNilPos = mustPos(t, testSrc, "p == nil", "nil")
	locs.testErrorPos = mustPos(t, testSrc, "func describeFromTest(err error) string {", "error")

	return root, locs
}

// builtinGoPath resolves the toolchain's $GOROOT/src/builtin/builtin.go
// path independently of golance's own goroot() cache
// (internal/langfeat/builtin.go), so this test's expectations track
// whatever the installed toolchain's real source says rather than a
// hardcoded, version-fragile path.
func builtinGoPath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "src", "builtin", "builtin.go")
}

// TestE2E_BuiltinNavigation covers "Go to Definition" and hover on a
// universe (predeclared) identifier — nil, error, len — through a real
// running golance session, from both a workspace file and a _test.go file:
// gopls resolves these against $GOROOT/src/builtin/builtin.go (see
// internal/langfeat/builtin.go's doc), and this pins that same behavior
// end to end.
func TestE2E_BuiltinNavigation(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EBuiltinNavModule(t)
	builtinGo := builtinGoPath(t)
	wantNilPos := declPosition(t, builtinGo, "nil")
	wantErrorPos := declPosition(t, builtinGo, "error")

	c := startClient(t, root)
	c.initialize(t, root)
	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	t.Run("workspace_file", func(t *testing.T) {
		checkE2EBuiltinNav(t, c, locs.appFile, locs.appNilPos, locs.appErrorPos, locs.appLenPos, builtinGo, wantNilPos, wantErrorPos)
	})

	c.openFile(t, locs.testFile)

	t.Run("test_file", func(t *testing.T) {
		checkE2EBuiltinNav(t, c, locs.testFile, locs.testNilPos, locs.testErrorPos, locs.testLenPos, builtinGo, wantNilPos, wantErrorPos)
	})
}

// checkE2EBuiltinNav runs the definition/hover assertions
// TestE2E_BuiltinNavigation needs for file: definition on nil and on error
// both land exactly at builtinGo's own declaring identifier (wantNilPos/
// wantErrorPos, ground truth from declPosition), and hover on len contains
// its builtin.go doc comment.
func checkE2EBuiltinNav(t *testing.T, c *lspClient, file string, nilPos, errorPos, lenPos protocol.Position, builtinGo string, wantNilPos, wantErrorPos protocol.Position) {
	t.Helper()

	t.Run("definition_nil", func(t *testing.T) {
		got := definitionAt(t, c, file, nilPos)
		if len(got) != 1 {
			t.Fatalf("definition(nil) = %+v, want exactly 1 location", got)
		}
		if got[0].URI.FsPath() != builtinGo {
			t.Fatalf("definition(nil) = %s, want %s", got[0].URI.FsPath(), builtinGo)
		}
		if got[0].Range.Start != wantNilPos {
			t.Errorf("definition(nil) landed at %+v, want %+v (nil's own declaring identifier)", got[0].Range.Start, wantNilPos)
		}
	})

	t.Run("definition_error", func(t *testing.T) {
		got := definitionAt(t, c, file, errorPos)
		if len(got) != 1 {
			t.Fatalf("definition(error) = %+v, want exactly 1 location", got)
		}
		if got[0].URI.FsPath() != builtinGo {
			t.Fatalf("definition(error) = %s, want %s", got[0].URI.FsPath(), builtinGo)
		}
		if got[0].Range.Start != wantErrorPos {
			t.Errorf("definition(error) landed at %+v, want %+v (error's own declaring identifier)", got[0].Range.Start, wantErrorPos)
		}
	})

	t.Run("hover_len", func(t *testing.T) {
		resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     lenPos,
			},
		}, e2eRequestBudget)
		if len(resp.Error) > 0 {
			t.Fatalf("hover(len) failed: %s", resp.Error)
		}
		var hover protocol.Hover
		if err := protocol.Unmarshal(resp.Result, &hover); err != nil {
			t.Fatalf("unmarshal hover result: %v", err)
		}
		md, ok := hover.Contents.(*protocol.MarkupContent)
		if !ok {
			t.Fatalf("hover contents type = %T, want *protocol.MarkupContent", hover.Contents)
		}
		if !strings.Contains(md.Value, "The len built-in function returns the length of v") {
			t.Errorf("hover(len) = %q, want it to contain len's builtin.go doc comment", md.Value)
		}
	})
}
