package langfeat_test

// This file pins two gopls-parity gaps found while auditing golance's
// informational/editing LSP features against a real gopls v0.23.0 (see
// ./audit-informational.md at the repo root for the full comparison and
// severity assessment). Both cases were verified by running gopls's own
// `symbols`/`semtok` CLI subcommands against these exact fixture files.

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

// TestSemanticTokens_PackageLevelConst_OverTaggedStatic pins a real
// mismatch: golance marks a package-level const's semantic token with
// BOTH ModReadonly and ModStatic (see semantic.go's staticModifiers,
// exercised here through internal/langfeat/testdata/module/auditfeat/
// consts.go's LevelDebug), and internal/langfeat/semantic_test.go's own
// TestSemanticTokens_ConstIsReadonlyAndStatic asserts that combination as
// intentional. `gopls semtok` disagrees: run against both this fixture and
// testdata/module/symbols/symbols.go, gopls tags a package-level CONST
// (MaxWidgets) "definition readonly" only — never "static" — while a
// package-level VAR (Count) gets "definition static" only, never
// "readonly". Root cause: objectKind (semantic.go) maps both *types.Const
// and an ordinary *types.Var to the same TokenVariable kind, and
// staticModifiers' ModStatic check keys only on that shared kind, not on
// the object actually being a non-const variable. Left unfixed here: doing
// so would need to also change semantic_test.go's existing assertion,
// which this audit's own ground rule (touch no existing test file) rules
// out — see audit-informational.md for the full writeup and a suggested
// one-line fix for whoever owns that test.
func TestSemanticTokens_PackageLevelConst_OverTaggedStatic(t *testing.T) {
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
	want := langfeat.ModDefinition | langfeat.ModReadonly | langfeat.ModStatic
	if tok.Modifiers != want {
		t.Fatalf("LevelDebug modifiers = %#x, want %#x (definition|readonly|static) — if this changed, golance now matches gopls and this test (and its doc comment) should be updated", tok.Modifiers, want)
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
