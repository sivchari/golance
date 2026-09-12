package langfeat_test

// This file covers two gopls-parity findings from auditing golance's
// informational/editing LSP features against a real gopls v0.23.0 (see
// ./audit-informational.md at the repo root for the full comparison and
// severity assessment): documentSymbol nesting (finding #9, now fixed —
// this test asserts the fixed, gopls-matching outline shape) and
// semanticTokens' const/static modifier (finding #11, fixed by this
// change — see TestSemanticTokens_PackageLevelConst_MatchesGoplsStatic's
// own doc). Both were verified by running gopls's own `symbols`/`semtok`
// CLI subcommands against these exact fixture files.

import (
	"bytes"
	"slices"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// TestDocumentSymbols_NestsStructFieldsAndInterfaceMembers pins the
// outline structure gopls produces: a struct's fields and an interface's
// method set are children of their declaring type, never top-level
// entries. Without them an outline or breadcrumbs consumer cannot navigate
// to a field or interface method at all.
func TestDocumentSymbols_NestsStructFieldsAndInterfaceMembers(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "auditfeat", "iface.go")

	syms, err := langfeat.DocumentSymbols(cp, path)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}

	config := findSymbol(syms, "Config")
	if config == nil {
		t.Fatal("Config type symbol not found")
	}
	if got, want := childNames(config.Children), []string{"Name", "Port"}; !slices.Equal(got, want) {
		t.Errorf("Config.Children = %v, want %v", got, want)
	}
	iface := findSymbol(syms, "Reader")
	if iface == nil {
		t.Fatal("Reader interface symbol not found")
	}
	if !slices.Contains(childNames(iface.Children), "Read") {
		t.Errorf("Reader.Children = %v, want it to contain Read", childNames(iface.Children))
	}

	for _, name := range []string{"Name", "Port", "Read"} {
		if findSymbol(syms, name) != nil {
			t.Errorf("DocumentSymbols contains a top-level %q symbol; fields and interface methods belong under their declaring type", name)
		}
	}
}

func childNames(syms []langfeat.Symbol) []string {
	names := make([]string, len(syms))
	for i, s := range syms {
		names[i] = s.Name
	}
	return names
}

// TestSemanticTokens_PackageLevelConst_MatchesGoplsStatic closes a
// previously deferred gopls-parity gap: golance used to mark a
// package-level const's semantic token with BOTH ModReadonly and
// ModStatic (see semantic.go's staticModifiers, exercised here through
// internal/langfeat/testdata/module/auditfeat/consts.go's LevelDebug).
// `gopls semtok`, run against both this fixture and
// testdata/module/symbols/symbols.go, tags a package-level CONST
// (MaxWidgets) "definition readonly" only — never "static" — while a
// package-level VAR (Count) gets "definition static" only, never
// "readonly". staticModifiers now gates its ModStatic check on the object
// NOT being a *types.Const (in addition to the existing TokenVariable/
// package-level checks), matching gopls exactly; this test, and
// internal/langfeat/semantic_test.go's renamed
// TestSemanticTokens_ConstIsReadonlyNotStatic, were both updated together
// with that fix — see audit-informational.md finding #11 for the original
// writeup.
func TestSemanticTokens_PackageLevelConst_MatchesGoplsStatic(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "auditfeat", "consts.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	toks, err := langfeat.SemanticTokens(cp, path, text)
	if err != nil {
		t.Fatalf("SemanticTokens: %v", err)
	}

	tok := definitionTokenAt(t, text, toks, "LevelDebug Level = iota", "LevelDebug")
	want := langfeat.ModDefinition | langfeat.ModReadonly
	if tok.Modifiers != want {
		t.Fatalf("LevelDebug modifiers = %#x, want %#x (definition|readonly, no static — matching gopls)", tok.Modifiers, want)
	}
}

// definitionTokenAt returns the Token in toks whose range exactly covers
// name within its first occurrence of lineSubstr — mirroring mustPos's own
// "line substring, then token within it" anchoring (see e2e_repo_test.go's
// mustPos and its documented doc-comment/code collision pitfall) so a
// declaration's own doc comment, which can repeat the same identifier
// name, never matches instead of the declaration itself.
func definitionTokenAt(t *testing.T, text []byte, toks []langfeat.Token, lineSubstr, name string) langfeat.Token {
	t.Helper()
	lineOff := mustIndex(t, text, lineSubstr)
	off := lineOff + mustIndexFrom(t, text[lineOff:], name)
	for _, tok := range toks {
		if tok.Range.StartOffset == off && tok.Range.EndOffset == off+len(name) {
			return tok
		}
	}
	t.Fatalf("no token exactly covering %q at offset %d", name, off)
	return langfeat.Token{}
}

// mustIndexFrom is mustIndex against a text slice already positioned at
// the right line, so its own failure message reports a location relative
// to that slice rather than the whole file.
func mustIndexFrom(t *testing.T, text []byte, substr string) int {
	t.Helper()
	i := bytes.Index(text, []byte(substr))
	if i < 0 {
		t.Fatalf("substring %q not found in:\n%s", substr, text)
	}
	return i
}
