package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// tokenAt returns the Token whose Range starts at the byte offset of the
// first occurrence of substr in text, failing the test if none is found.
func tokenAt(t *testing.T, toks []langfeat.Token, text []byte, substr string) langfeat.Token {
	t.Helper()
	offset := mustIndex(t, text, substr)
	for _, tok := range toks {
		if tok.Range.StartOffset == offset {
			return tok
		}
	}
	t.Fatalf("no token starts at offset %d (%q); tokens: %+v", offset, substr, toks)
	return langfeat.Token{}
}

func semanticTokens(t *testing.T, pkgDir, file string) ([]langfeat.Token, []byte, string) {
	t.Helper()
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, pkgDir, file)
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	toks, err := langfeat.SemanticTokens(cp, path, text)
	if err != nil {
		t.Fatalf("SemanticTokens: %v", err)
	}
	return toks, text, path
}

func TestSemanticTokens_PackageClause(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	tok := tokenAt(t, toks, text, "semantic\n\nimport")
	if tok.Kind != langfeat.TokenNamespace || tok.Modifiers != langfeat.ModDefinition {
		t.Errorf("package clause ident = %+v, want Kind=TokenNamespace Modifiers=ModDefinition", tok)
	}
}

func TestSemanticTokens_InterfaceAndStruct(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")

	shape := tokenAt(t, toks, text, "Shape interface")
	if shape.Kind != langfeat.TokenInterface || shape.Modifiers != langfeat.ModDefinition {
		t.Errorf("Shape = %+v, want Kind=TokenInterface Modifiers=ModDefinition", shape)
	}

	rect := tokenAt(t, toks, text, "Rect struct")
	if rect.Kind != langfeat.TokenStruct || rect.Modifiers != langfeat.ModDefinition {
		t.Errorf("Rect = %+v, want Kind=TokenStruct Modifiers=ModDefinition", rect)
	}

	width := tokenAt(t, toks, text, "Width, Height")
	if width.Kind != langfeat.TokenProperty || width.Modifiers != langfeat.ModDefinition {
		t.Errorf("Width field decl = %+v, want Kind=TokenProperty Modifiers=ModDefinition", width)
	}
}

func TestSemanticTokens_MethodAndReceiver(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")

	recv := tokenAt(t, toks, text, "r Rect) Area")
	if recv.Kind != langfeat.TokenParameter || recv.Modifiers != langfeat.ModDefinition {
		t.Errorf("receiver decl = %+v, want Kind=TokenParameter Modifiers=ModDefinition", recv)
	}

	area := tokenAt(t, toks, text, "Area() float64")
	if area.Kind != langfeat.TokenMethod || area.Modifiers != langfeat.ModDefinition {
		t.Errorf("Area decl = %+v, want Kind=TokenMethod Modifiers=ModDefinition", area)
	}

	// Inside the body, "r" refers back to the receiver (still a
	// TokenParameter, same as its declaring occurrence) but this is a use,
	// not the definition, so it carries no ModDefinition.
	use := tokenAt(t, toks, text, "r.Width * r.Height")
	if use.Kind != langfeat.TokenParameter || use.Modifiers != 0 {
		t.Errorf("receiver use = %+v, want Kind=TokenParameter Modifiers=0", use)
	}
	field := tokenAt(t, toks, text, "Width * r.Height")
	if field.Kind != langfeat.TokenProperty || field.Modifiers != 0 {
		t.Errorf("Width field use = %+v, want Kind=TokenProperty Modifiers=0", field)
	}
}

func TestSemanticTokens_ConstIsReadonlyAndStatic(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	tok := tokenAt(t, toks, text, "MaxShapes = 16")
	want := langfeat.ModDefinition | langfeat.ModReadonly | langfeat.ModStatic
	if tok.Kind != langfeat.TokenVariable || tok.Modifiers != want {
		t.Errorf("MaxShapes = %+v, want Kind=TokenVariable Modifiers=%v (definition|readonly|static)", tok, want)
	}
}

func TestSemanticTokens_PackageVarIsStaticNotReadonly(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	tok := tokenAt(t, toks, text, "count int")
	want := langfeat.ModDefinition | langfeat.ModStatic
	if tok.Kind != langfeat.TokenVariable || tok.Modifiers != want {
		t.Errorf("count = %+v, want Kind=TokenVariable Modifiers=%v (definition|static, no readonly)", tok, want)
	}
}

func TestSemanticTokens_LocalVarIsNeitherStaticNorReadonly(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	tok := tokenAt(t, toks, text, "msg := fmt.Sprintf")
	if tok.Kind != langfeat.TokenVariable || tok.Modifiers != langfeat.ModDefinition {
		t.Errorf("msg local var = %+v, want Kind=TokenVariable Modifiers=ModDefinition (no static, no readonly)", tok)
	}
}

func TestSemanticTokens_GenericTypeParameter(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")

	decl := tokenAt(t, toks, text, "T any]")
	if decl.Kind != langfeat.TokenTypeParameter || decl.Modifiers != langfeat.ModDefinition {
		t.Errorf("T decl = %+v, want Kind=TokenTypeParameter Modifiers=ModDefinition", decl)
	}
	anyTok := tokenAt(t, toks, text, "any](xs")
	if anyTok.Kind != langfeat.TokenInterface || anyTok.Modifiers != langfeat.ModDefaultLibrary {
		t.Errorf("any = %+v, want Kind=TokenInterface Modifiers=ModDefaultLibrary", anyTok)
	}
	use := tokenAt(t, toks, text, "T {\n\treturn")
	if use.Kind != langfeat.TokenTypeParameter || use.Modifiers != 0 {
		t.Errorf("T return type = %+v, want Kind=TokenTypeParameter Modifiers=0 (a use, not the definition)", use)
	}
}

func TestSemanticTokens_StdlibPackageAndFunc(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")

	pkg := tokenAt(t, toks, text, "fmt.Sprintf")
	if pkg.Kind != langfeat.TokenNamespace || pkg.Modifiers != langfeat.ModDefaultLibrary {
		t.Errorf("fmt = %+v, want Kind=TokenNamespace Modifiers=ModDefaultLibrary", pkg)
	}
	fn := tokenAt(t, toks, text, "Sprintf(\"rect")
	if fn.Kind != langfeat.TokenFunction || fn.Modifiers != langfeat.ModDefaultLibrary {
		t.Errorf("fmt.Sprintf = %+v, want Kind=TokenFunction Modifiers=ModDefaultLibrary", fn)
	}
}

func TestSemanticTokens_Builtin(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	fn := tokenAt(t, toks, text, "len(s)")
	if fn.Kind != langfeat.TokenFunction || fn.Modifiers != langfeat.ModDefaultLibrary {
		t.Errorf("len builtin = %+v, want Kind=TokenFunction Modifiers=ModDefaultLibrary", fn)
	}
}

func TestSemanticTokens_DeprecatedFunc(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	tok := tokenAt(t, toks, text, "Describe(r Rect)")
	want := langfeat.ModDefinition | langfeat.ModDeprecated
	if tok.Kind != langfeat.TokenFunction || tok.Modifiers != want {
		t.Errorf("Describe = %+v, want Kind=TokenFunction Modifiers=%v (definition|deprecated)", tok, want)
	}
}

func TestSemanticTokens_UnexportedFunc(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	fn := tokenAt(t, toks, text, "lengthOf(s string)")
	if fn.Kind != langfeat.TokenFunction || fn.Modifiers != langfeat.ModDefinition {
		t.Errorf("lengthOf = %+v, want Kind=TokenFunction Modifiers=ModDefinition", fn)
	}
	param := tokenAt(t, toks, text, "s string) int")
	if param.Kind != langfeat.TokenParameter || param.Modifiers != langfeat.ModDefinition {
		t.Errorf("lengthOf's s param = %+v, want Kind=TokenParameter Modifiers=ModDefinition", param)
	}
}

func TestSemanticTokens_LexicalKinds(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")

	kw := tokenAt(t, toks, text, "package semantic")
	if kw.Kind != langfeat.TokenKeyword {
		t.Errorf("\"package\" keyword = %+v, want Kind=TokenKeyword", kw)
	}
	cm := tokenAt(t, toks, text, "// Package semantic")
	if cm.Kind != langfeat.TokenComment {
		t.Errorf("doc comment = %+v, want Kind=TokenComment", cm)
	}
	str := tokenAt(t, toks, text, `"fmt"`)
	if str.Kind != langfeat.TokenString {
		t.Errorf(`"fmt" import = %+v, want Kind=TokenString`, str)
	}
	num := tokenAt(t, toks, text, "16")
	if num.Kind != langfeat.TokenNumber {
		t.Errorf("16 literal = %+v, want Kind=TokenNumber", num)
	}
	op := tokenAt(t, toks, text, "* r.Height")
	if op.Kind != langfeat.TokenOperator {
		t.Errorf("\"*\" operator = %+v, want Kind=TokenOperator", op)
	}
}

func TestSemanticTokens_NoTokenForPunctuationOrBlank(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	// "(" right after "Area" must not have its own token: punctuation is
	// excluded (see lexicalKind).
	paren := mustIndex(t, text, "Area() float64")
	for _, tok := range toks {
		if tok.Range.StartOffset == paren+len("Area") {
			t.Errorf("unexpected token for \"(\": %+v", tok)
		}
	}
}

func TestSemanticTokens_SortedAndNonOverlapping(t *testing.T) {
	toks, text, _ := semanticTokens(t, "semantic", "semantic.go")
	if len(toks) == 0 {
		t.Fatal("SemanticTokens returned no tokens")
	}
	for i, tok := range toks {
		if tok.Range.StartOffset >= tok.Range.EndOffset {
			t.Errorf("toks[%d] = %+v, has a non-positive-length range", i, tok)
		}
		for j := tok.Range.StartOffset; j < tok.Range.EndOffset; j++ {
			if text[j] == '\n' {
				t.Errorf("toks[%d] = %+v, spans a line break", i, tok)
			}
		}
		if i == 0 {
			continue
		}
		prev := toks[i-1]
		if tok.Range.StartOffset < prev.Range.EndOffset {
			t.Errorf("toks[%d] = %+v overlaps toks[%d] = %+v", i, tok, i-1, prev)
		}
	}
}

// TestSemanticTokens_TypeErrorFile verifies the "型検査が失敗しているファイル
// でも、parse できた範囲でキーワード・コメント・リテラル等の構文的トークンは
// 返す" requirement: broken.go has an unresolved selector (a type error) and
// a dangling "." (recovered by the parser into a partial AST), yet its
// lexical tokens still come through.
func TestSemanticTokens_TypeErrorFile(t *testing.T) {
	toks, text, _ := semanticTokens(t, "broken", "broken.go")
	if len(toks) == 0 {
		t.Fatal("SemanticTokens returned no tokens for a file with a type error")
	}

	kw := tokenAt(t, toks, text, "type Foo struct")
	if kw.Kind != langfeat.TokenKeyword {
		t.Errorf("\"type\" keyword = %+v, want Kind=TokenKeyword", kw)
	}
	cm := tokenAt(t, toks, text, "// Foo has a Bar field.")
	if cm.Kind != langfeat.TokenComment {
		t.Errorf("doc comment = %+v, want Kind=TokenComment", cm)
	}
	str := tokenAt(t, toks, text, `"strings"`)
	if str.Kind != langfeat.TokenString {
		t.Errorf(`"strings" import = %+v, want Kind=TokenString`, str)
	}
}
