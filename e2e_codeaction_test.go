package golance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// caLocs records the file paths the code-action E2E subtests exercise,
// captured while the synthetic module is written.
type caLocs struct {
	libFile          string // lib/lib.go, exports Greet
	organizeFile     string // organize/organize.go, imports out of order
	unusedImportFile string // unusedimport/unusedimport.go, one unused import
	unusedVarFile    string // unusedvar/unusedvar.go, one unused short-declared var
	undefinedFile    string // undefined/undefined.go, calls lib.Greet unqualified and unimported
}

// writeCodeActionModule writes a small synthetic module dedicated to the
// textDocument/codeAction E2E suite: one library package plus one file per
// fixable diagnostic, kept separate so each subtest's diagnostic is the
// only one in its file.
func writeCodeActionModule(t *testing.T) (root string, locs caLocs) {
	t.Helper()
	root = t.TempDir()

	write := func(rel, content string) string {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		return path
	}

	write("go.mod", "module example.com/codeaction\n\ngo 1.21\n")

	locs.libFile = write("lib/lib.go", "package lib\n\n// Greet returns a greeting.\nfunc Greet() string {\n\treturn \"hi\"\n}\n")

	locs.organizeFile = write("organize/organize.go", "package organize\n\nimport (\n\t\"strconv\"\n\t\"fmt\"\n)\n\nfunc F() string {\n\treturn fmt.Sprintf(\"%s\", strconv.Itoa(1))\n}\n")

	locs.unusedImportFile = write("unusedimport/unusedimport.go", "package unusedimport\n\nimport \"fmt\"\n\nfunc F() int {\n\treturn 1\n}\n")

	locs.unusedVarFile = write("unusedvar/unusedvar.go", "package unusedvar\n\nfunc F() {\n\tx := 1\n\t_ = 2\n}\n")

	locs.undefinedFile = write("undefined/undefined.go", "package undefined\n\nfunc F() string {\n\treturn Greet()\n}\n")

	return root, locs
}

// readFixture reads a synthetic test file this suite itself just wrote
// (see writeCodeActionModule), for comparing against a code action's edit.
func readFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestE2ECodeAction drives textDocument/codeAction against a real golance
// binary: source.organizeImports, the three quickfix kinds, and
// Context.Only filtering.
func TestE2ECodeAction(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeCodeActionModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	t.Run("organize_imports", func(t *testing.T) {
		checkE2ECodeActionOrganizeImports(t, c, locs.organizeFile)
	})

	var unusedImportDiag protocol.Diagnostic
	t.Run("unused_import", func(t *testing.T) {
		unusedImportDiag = checkE2ECodeActionUnusedImport(t, c, locs.unusedImportFile)
	})

	t.Run("unused_var", func(t *testing.T) {
		checkE2ECodeActionUnusedVar(t, c, locs.unusedVarFile)
	})

	// The undefined-symbol quickfix needs the facts index; opening a file
	// starts the indexer subprocess, so wait for it before relying on the
	// import-candidate lookup.
	c.openFile(t, locs.libFile)
	c.waitForIndexReady(t)

	t.Run("undefined_symbol_adds_import", func(t *testing.T) {
		checkE2ECodeActionUndefinedSymbol(t, c, locs.undefinedFile)
	})

	t.Run("only_filters_by_kind", func(t *testing.T) {
		checkE2ECodeActionOnlyFilter(t, c, locs.unusedImportFile, &unusedImportDiag)
	})
}

// codeActionRequest builds a textDocument/codeAction request for path,
// scoped to rng and the given diagnostics/only filter.
func codeActionRequest(path string, rng protocol.Range, diags []protocol.Diagnostic, only []protocol.CodeActionKind) *protocol.CodeActionParams {
	return &protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
		Range:        rng,
		Context:      protocol.CodeActionContext{Diagnostics: diags, Only: only},
	}
}

// callCodeAction sends the request and decodes the response into a
// []protocol.CodeAction.
func callCodeAction(t *testing.T, c *lspClient, params *protocol.CodeActionParams) []protocol.CodeAction {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentCodeAction, params, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("textDocument/codeAction failed: %s", resp.Error)
	}
	var actions []protocol.CodeAction
	if err := protocol.Unmarshal(resp.Result, &actions); err != nil {
		t.Fatalf("unmarshal codeAction result: %v", err)
	}
	return actions
}

// applyFirstEdit returns text with action's edits for path applied.
func applyFirstEdit(t *testing.T, path, text string, action *protocol.CodeAction) string {
	t.Helper()
	if action.Edit == nil {
		t.Fatal("action.Edit is nil")
	}
	edits := action.Edit.Changes[uri.File(path)]
	if len(edits) == 0 {
		t.Fatalf("no edits for %s in action %q", path, action.Title)
	}
	return applyLSPEdits(text, edits)
}

// applyLSPEdits applies edits (line/character-addressed, 0-based UTF-16 —
// this suite's fixtures are ASCII-only, so UTF-16 offsets and byte offsets
// coincide) to text, in descending position order so earlier offsets stay
// valid.
func applyLSPEdits(text string, edits []protocol.TextEdit) string {
	lines := strings.Split(text, "\n")
	type span struct {
		start, end int
		newText    string
	}
	spans := make([]span, len(edits))
	for i, e := range edits {
		spans[i] = span{
			start:   offsetForPosition(lines, e.Range.Start),
			end:     offsetForPosition(lines, e.Range.End),
			newText: e.NewText,
		}
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[j].start > spans[i].start {
				spans[i], spans[j] = spans[j], spans[i]
			}
		}
	}
	b := []byte(text)
	for _, s := range spans {
		tail := append([]byte(s.newText), b[s.end:]...)
		b = append(b[:s.start], tail...)
	}
	return string(b)
}

// offsetForPosition converts a 0-based line/character position (ASCII
// fixtures only, so character == byte column) into a byte offset into the
// text lines were split from.
func offsetForPosition(lines []string, pos protocol.Position) int {
	off := 0
	for i := uint32(0); i < pos.Line; i++ {
		off += len(lines[i]) + 1
	}
	return off + int(pos.Character)
}

func checkE2ECodeActionOrganizeImports(t *testing.T, c *lspClient, path string) {
	t.Helper()
	c.openFile(t, path)
	text := readFixture(t, path)

	actions := callCodeAction(t, c, codeActionRequest(path, protocol.Range{}, nil, []protocol.CodeActionKind{protocol.CodeActionKindSourceOrganizeImports}))
	var found *protocol.CodeAction
	for i := range actions {
		if actions[i].Kind != nil && *actions[i].Kind == protocol.CodeActionKindSourceOrganizeImports {
			found = &actions[i]
		}
	}
	if found == nil {
		t.Fatalf("no source.organizeImports action in %+v", actions)
	}

	got := applyFirstEdit(t, path, text, found)
	fmtIdx := strings.Index(got, `"fmt"`)
	strconvIdx := strings.Index(got, `"strconv"`)
	if fmtIdx < 0 || strconvIdx < 0 {
		t.Fatalf("organized result missing an import: %s", got)
	}
	if fmtIdx > strconvIdx {
		t.Errorf("organized result = %s, want \"fmt\" sorted before \"strconv\"", got)
	}
}

func checkE2ECodeActionUnusedImport(t *testing.T, c *lspClient, path string) protocol.Diagnostic {
	t.Helper()
	c.openFile(t, path)
	diags := c.waitForDiagnostics(t, path)
	diag := findDiagnosticContaining(t, diags, "imported and not used")
	text := readFixture(t, path)

	actions := callCodeAction(t, c, codeActionRequest(path, diag.Range, []protocol.Diagnostic{diag}, []protocol.CodeActionKind{protocol.CodeActionKindQuickFix}))
	if len(actions) == 0 {
		t.Fatal("no quickfix actions for the unused-import diagnostic")
	}
	got := applyFirstEdit(t, path, text, &actions[0])
	if strings.Contains(got, `"fmt"`) {
		t.Errorf("result still imports fmt: %s", got)
	}
	return diag
}

func checkE2ECodeActionUnusedVar(t *testing.T, c *lspClient, path string) {
	t.Helper()
	c.openFile(t, path)
	diags := c.waitForDiagnostics(t, path)
	diag := findDiagnosticContaining(t, diags, "declared and not used")
	text := readFixture(t, path)

	actions := callCodeAction(t, c, codeActionRequest(path, diag.Range, []protocol.Diagnostic{diag}, []protocol.CodeActionKind{protocol.CodeActionKindQuickFix}))
	if len(actions) == 0 {
		t.Fatal("no quickfix actions for the unused-var diagnostic")
	}
	got := applyFirstEdit(t, path, text, &actions[0])
	want := "package unusedvar\n\nfunc F() {\n\t_ = 1\n\t_ = 2\n}\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func checkE2ECodeActionUndefinedSymbol(t *testing.T, c *lspClient, path string) {
	t.Helper()
	c.openFile(t, path)
	diags := c.waitForDiagnostics(t, path)
	diag := findDiagnosticContaining(t, diags, "undefined: Greet")
	text := readFixture(t, path)

	actions := callCodeAction(t, c, codeActionRequest(path, diag.Range, []protocol.Diagnostic{diag}, []protocol.CodeActionKind{protocol.CodeActionKindQuickFix}))
	if len(actions) == 0 {
		t.Fatal("no quickfix actions for the undefined-symbol diagnostic")
	}
	got := applyFirstEdit(t, path, text, &actions[0])
	if !strings.Contains(got, `"example.com/codeaction/lib"`) {
		t.Errorf("result = %s, want it to import example.com/codeaction/lib", got)
	}
	if !strings.Contains(got, "lib.Greet()") {
		t.Errorf("result = %s, want it to call lib.Greet()", got)
	}
}

func checkE2ECodeActionOnlyFilter(t *testing.T, c *lspClient, path string, diag *protocol.Diagnostic) {
	t.Helper()

	quickFixOnly := callCodeAction(t, c, codeActionRequest(path, diag.Range, []protocol.Diagnostic{*diag}, []protocol.CodeActionKind{protocol.CodeActionKindQuickFix}))
	for i := range quickFixOnly {
		if quickFixOnly[i].Kind != nil && *quickFixOnly[i].Kind == protocol.CodeActionKindSourceOrganizeImports {
			t.Errorf("Only=[quickfix] returned a source.organizeImports action: %+v", quickFixOnly[i])
		}
	}

	organizeOnly := callCodeAction(t, c, codeActionRequest(path, diag.Range, []protocol.Diagnostic{*diag}, []protocol.CodeActionKind{protocol.CodeActionKindSourceOrganizeImports}))
	for i := range organizeOnly {
		if organizeOnly[i].Kind != nil && *organizeOnly[i].Kind == protocol.CodeActionKindQuickFix {
			t.Errorf("Only=[source.organizeImports] returned a quickfix action: %+v", organizeOnly[i])
		}
	}
}

// findDiagnosticContaining returns the first diagnostic in diags whose
// message contains substr, failing t if none matches.
func findDiagnosticContaining(t *testing.T, diags []protocol.Diagnostic, substr string) protocol.Diagnostic {
	t.Helper()
	for i := range diags {
		s, ok := diags[i].Message.(protocol.String)
		if ok && strings.Contains(string(s), substr) {
			return diags[i]
		}
	}
	t.Fatalf("no diagnostic containing %q in %+v", substr, diags)
	return protocol.Diagnostic{}
}
