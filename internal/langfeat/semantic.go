package langfeat

import (
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sivchari/golance/internal/check"
)

// TokenKind categorizes a semantic Token, independent of the LSP wire
// encoding. Its value doubles as the token's LSP tokenType index: order
// here must match TokenKindNames, which the server advertises as
// SemanticTokensLegend.TokenTypes.
type TokenKind uint32

// Kinds a Token can have. This is the subset of LSP's predefined semantic
// token types that has a meaningful Go counterpart — Go has no classes,
// enums, or macros, so those LSP-defined types are omitted.
const (
	TokenNamespace TokenKind = iota
	TokenType
	TokenInterface
	TokenStruct
	TokenTypeParameter
	TokenParameter
	TokenVariable
	TokenProperty
	TokenFunction
	TokenMethod
	TokenKeyword
	TokenComment
	TokenString
	TokenNumber
	TokenOperator
)

// TokenKindNames are the LSP token type names for each TokenKind, in index
// order.
var TokenKindNames = []string{
	"namespace",
	"type",
	"interface",
	"struct",
	"typeParameter",
	"parameter",
	"variable",
	"property",
	"function",
	"method",
	"keyword",
	"comment",
	"string",
	"number",
	"operator",
}

// TokenModifier is a bitset of modifiers on a Token. Bit N corresponds to
// TokenModifierNames[N], the name the server advertises at that position in
// SemanticTokensLegend.TokenModifiers.
type TokenModifier uint32

// Modifiers a Token can carry.
const (
	// ModDeclaration marks a symbol's forward declaration: a func with no
	// body (implemented elsewhere, e.g. in assembly).
	ModDeclaration TokenModifier = 1 << iota
	// ModDefinition marks the occurrence that introduces a symbol: the name
	// in a type/func/var/const/field/parameter declaration.
	ModDefinition
	// ModReadonly marks a constant.
	ModReadonly
	// ModStatic marks a package-level (as opposed to local) variable or
	// constant.
	ModStatic
	// ModDeprecated marks a symbol documented with a "Deprecated:" comment
	// (https://go.dev/wiki/Deprecated), checkable only for symbols declared
	// in the package being queried — see isDeprecated.
	ModDeprecated
	// ModDefaultLibrary marks a predeclared (universe-scope) identifier or
	// one from the standard library.
	ModDefaultLibrary
)

// TokenModifierNames are the LSP token modifier names for each
// TokenModifier bit, in bit-index order.
var TokenModifierNames = []string{
	"declaration",
	"definition",
	"readonly",
	"static",
	"deprecated",
	"defaultLibrary",
}

// Token is one classified span of source: a byte-offset Range tagged with
// its semantic Kind and Modifiers. Every Token is confined to a single
// line — SemanticTokens splits any lexical token (a block comment or raw
// string literal) that spans more than one line into several single-line
// Tokens, since Encode's delta-line/delta-char encoding assumes that.
type Token struct {
	Range     Range
	Kind      TokenKind
	Modifiers TokenModifier
}

// SemanticTokens classifies file's source into semantic Tokens, sorted by
// Range.StartOffset: keywords, operators, comments, and string/number
// literals from a lexical scan of text (independent of type-checking, so
// these are returned even for a file with parse or type errors), plus
// identifiers classified using cp's type information (package names,
// types, interfaces, structs, type parameters, parameters, variables,
// fields, functions, and methods). text must be exactly the content cp was
// checked against.
func SemanticTokens(cp *check.CheckedPackage, file string, text []byte) ([]Token, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return nil, err
	}
	if len(text) != tf.Size() {
		return nil, fmt.Errorf("langfeat: text length %d does not match %s size %d", len(text), tf.Name(), tf.Size())
	}

	toks := lexicalTokens(tf, text)
	toks = append(toks, identTokens(cp, astFile, tf)...)

	split := make([]Token, 0, len(toks))
	for _, t := range toks {
		split = append(split, splitMultilineToken(t, text)...)
	}
	sort.Slice(split, func(i, j int) bool { return split[i].Range.StartOffset < split[j].Range.StartOffset })
	return split, nil
}

// lexicalTokens scans tf's source text for keywords, operators, comments,
// and string/number literals, independent of whether the file parses or
// type-checks.
func lexicalTokens(tf *token.File, text []byte) []Token {
	var s scanner.Scanner
	s.Init(tf, text, func(token.Position, string) {}, scanner.ScanComments)

	var toks []Token
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		kind, ok := lexicalKind(tok)
		if !ok {
			continue
		}
		start := tf.Offset(pos)
		toks = append(toks, Token{Range: Range{StartOffset: start, EndOffset: start + lexicalLen(tok, lit)}, Kind: kind})
	}
	return toks
}

// lexicalKind maps a non-identifier go/scanner token to its TokenKind.
// Punctuation (parens, braces, comma, period, semicolon, colon) has no
// token, since editors already render it via ordinary (non-semantic)
// syntax highlighting.
func lexicalKind(tok token.Token) (TokenKind, bool) {
	switch {
	case tok == token.COMMENT:
		return TokenComment, true
	case tok == token.STRING || tok == token.CHAR:
		return TokenString, true
	case tok == token.INT || tok == token.FLOAT || tok == token.IMAG:
		return TokenNumber, true
	case tok.IsKeyword():
		return TokenKeyword, true
	case isOperatorToken(tok):
		return TokenOperator, true
	default:
		return 0, false
	}
}

// isOperatorToken reports whether tok is a genuine operator, as opposed to
// punctuation that go/token.Token.IsOperator also classifies as an
// "operator" (parens, braces, comma, period, semicolon, colon).
func isOperatorToken(tok token.Token) bool {
	switch tok {
	case token.ADD, token.SUB, token.MUL, token.QUO, token.REM,
		token.AND, token.OR, token.XOR, token.SHL, token.SHR, token.AND_NOT,
		token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN, token.REM_ASSIGN,
		token.AND_ASSIGN, token.OR_ASSIGN, token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN, token.AND_NOT_ASSIGN,
		token.LAND, token.LOR, token.ARROW, token.INC, token.DEC,
		token.EQL, token.LSS, token.GTR, token.ASSIGN, token.NOT,
		token.NEQ, token.LEQ, token.GEQ, token.DEFINE, token.ELLIPSIS, token.TILDE:
		return true
	default:
		return false
	}
}

// lexicalLen returns tok's source length in bytes: the literal text's own
// length for literals and comments (lit is the raw scanned source, quotes
// and comment markers included), or the token's canonical spelling for
// keywords and operators (token.Token.String() returns exactly that source
// spelling for every non-literal token).
func lexicalLen(tok token.Token, lit string) int {
	switch tok {
	case token.COMMENT, token.STRING, token.CHAR, token.INT, token.FLOAT, token.IMAG:
		return len(lit)
	default:
		return len(tok.String())
	}
}

// splitMultilineToken splits t into one Token per line if its range spans
// more than one line (only possible for a block comment or a raw string
// literal), since Encode's delta encoding assumes every Token is confined
// to a single line. A single-line t is returned unchanged. Zero-length
// pieces (e.g. the line break itself) are dropped.
func splitMultilineToken(t Token, text []byte) []Token {
	start, end := t.Range.StartOffset, t.Range.EndOffset
	var out []Token
	lineStart := start
	for i := start; i < end; i++ {
		if text[i] == '\n' {
			if lineStart < i {
				out = append(out, Token{Range: Range{StartOffset: lineStart, EndOffset: i}, Kind: t.Kind, Modifiers: t.Modifiers})
			}
			lineStart = i + 1
		}
	}
	if lineStart < end {
		out = append(out, Token{Range: Range{StartOffset: lineStart, EndOffset: end}, Kind: t.Kind, Modifiers: t.Modifiers})
	}
	return out
}

// identTokens classifies every resolvable *ast.Ident in astFile using cp's
// type information.
func identTokens(cp *check.CheckedPackage, astFile *ast.File, tf *token.File) []Token {
	paramObjs := collectParamObjects(cp, astFile)
	bodyless := bodylessFuncIdents(astFile)
	modCache := make(map[types.Object]TokenModifier)

	var toks []Token
	ast.Inspect(astFile, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name == "_" {
			return true
		}
		t, ok := classifyIdent(cp, astFile, id, paramObjs, bodyless, modCache)
		if !ok {
			return true
		}
		t.Range = rangeOf(tf, id.Pos(), id.End())
		toks = append(toks, t)
		return true
	})
	return toks
}

// classifyIdent resolves id to its Token, or ok=false if id does not
// resolve to a kind SemanticTokens tracks (e.g. a goto label, the
// predeclared nil identifier, or an identifier left unresolved by a type
// error).
func classifyIdent(cp *check.CheckedPackage, astFile *ast.File, id *ast.Ident, paramObjs map[types.Object]bool, bodyless map[*ast.Ident]bool, modCache map[types.Object]TokenModifier) (Token, bool) {
	if id == astFile.Name {
		return Token{Kind: TokenNamespace, Modifiers: ModDefinition}, true
	}
	obj := cp.Info().ObjectOf(id)
	if obj == nil {
		return Token{}, false
	}
	kind, ok := objectKind(obj, paramObjs)
	if !ok {
		return Token{}, false
	}

	mods, cached := modCache[obj]
	if !cached {
		mods = staticModifiers(cp, obj, kind)
		modCache[obj] = mods
	}
	if def := cp.Info().Defs[id]; def != nil {
		if bodyless[id] {
			mods |= ModDeclaration
		} else {
			mods |= ModDefinition
		}
	}
	return Token{Kind: kind, Modifiers: mods}, true
}

// objectKind maps obj to its TokenKind, or ok=false for an object kind
// SemanticTokens does not classify (e.g. *types.Label or *types.Nil).
func objectKind(obj types.Object, paramObjs map[types.Object]bool) (TokenKind, bool) {
	switch o := obj.(type) {
	case *types.PkgName:
		return TokenNamespace, true
	case *types.TypeName:
		return typeNameKind(o), true
	case *types.Func:
		return funcKind(o), true
	case *types.Builtin:
		return TokenFunction, true
	case *types.Const:
		return TokenVariable, true
	case *types.Var:
		return varKind(o, paramObjs), true
	default:
		return 0, false
	}
}

// typeNameKind classifies a *types.TypeName by its underlying type: a type
// parameter, an interface (including "any" and "error"), a struct, or
// TokenType as the fallback for every other named or basic type.
func typeNameKind(o *types.TypeName) TokenKind {
	if _, ok := o.Type().(*types.TypeParam); ok {
		return TokenTypeParameter
	}
	switch o.Type().Underlying().(type) {
	case *types.Interface:
		return TokenInterface
	case *types.Struct:
		return TokenStruct
	default:
		return TokenType
	}
}

// funcKind distinguishes a method (a *types.Func with a receiver) from a
// plain function.
func funcKind(o *types.Func) TokenKind {
	if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
		return TokenMethod
	}
	return TokenFunction
}

// varKind distinguishes a struct field, a function/method parameter (or
// receiver, tracked the same way in paramObjs), and an ordinary variable.
func varKind(o *types.Var, paramObjs map[types.Object]bool) TokenKind {
	if o.IsField() {
		return TokenProperty
	}
	if paramObjs[o] {
		return TokenParameter
	}
	return TokenVariable
}

// collectParamObjects returns the set of *types.Var objects bound by a
// function or method's receiver, parameters, or results anywhere in
// astFile (including nested function literals), so every occurrence of
// such a variable — not just its binding occurrence — classifies as
// TokenParameter.
func collectParamObjects(cp *check.CheckedPackage, astFile *ast.File) map[types.Object]bool {
	objs := make(map[types.Object]bool)
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, name := range f.Names {
				if obj := cp.Info().ObjectOf(name); obj != nil {
					objs[obj] = true
				}
			}
		}
	}
	ast.Inspect(astFile, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			addFields(d.Recv)
		case *ast.FuncType:
			addFields(d.Params)
			addFields(d.Results)
		}
		return true
	})
	return objs
}

// bodylessFuncIdents returns the set of *ast.FuncDecl.Name identifiers for
// top-level funcs declared with no body (implemented elsewhere, e.g. in
// assembly) — their defining occurrence gets ModDeclaration rather than
// ModDefinition. Only *ast.FuncDecl can lack a body; a function literal's
// body is mandatory Go syntax.
func bodylessFuncIdents(astFile *ast.File) map[*ast.Ident]bool {
	m := make(map[*ast.Ident]bool)
	for _, d := range astFile.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body == nil {
			m[fd.Name] = true
		}
	}
	return m
}

// staticModifiers computes obj's position-independent modifiers: readonly,
// static, deprecated, and defaultLibrary. These depend only on obj, not on
// which of its occurrences is being classified, unlike ModDeclaration/
// ModDefinition — so callers may cache the result per obj.
func staticModifiers(cp *check.CheckedPackage, obj types.Object, kind TokenKind) TokenModifier {
	var m TokenModifier
	if _, ok := obj.(*types.Const); ok {
		m |= ModReadonly
	}
	if kind == TokenVariable && isPackageLevel(obj) {
		m |= ModStatic
	}
	if canBeDeprecated(kind, obj) && isDeprecated(cp, obj) {
		m |= ModDeprecated
	}
	if isDefaultLibrary(obj) {
		m |= ModDefaultLibrary
	}
	return m
}

// isPackageLevel reports whether obj is declared directly in its
// package's scope, as opposed to a local (function-body) scope.
func isPackageLevel(obj types.Object) bool {
	pkg := obj.Pkg()
	return pkg != nil && obj.Parent() == pkg.Scope()
}

// canBeDeprecated reports whether kind is a symbol kind that can carry its
// own "Deprecated:" doc comment. Parameters, type parameters, and local
// variables have no declaring node of their own for docForObject to
// search from — it would instead fall back to the doc comment of whatever
// declaration enclosing them happens to have one (e.g. the func they are
// declared in), misattributing that declaration's deprecation to them.
func canBeDeprecated(kind TokenKind, obj types.Object) bool {
	switch kind {
	case TokenParameter, TokenTypeParameter:
		return false
	case TokenVariable:
		return isPackageLevel(obj)
	default:
		return true
	}
}

// isDeprecated reports whether obj's doc comment follows the "Deprecated:"
// convention (https://go.dev/wiki/Deprecated). Only checkable for objects
// declared in cp's own package — docForObject returns "" for anything
// else, since only export data (no source) is available for it.
func isDeprecated(cp *check.CheckedPackage, obj types.Object) bool {
	return strings.Contains(docForObject(cp, obj), "Deprecated:")
}

// isDefaultLibrary reports whether obj is predeclared (obj.Pkg() == nil,
// e.g. len, error, int) or belongs to a standard-library package. A
// *types.PkgName is special-cased: its Pkg() is the *importing* package
// (whatever file the import appears in), not the package it names, so the
// package it names — reported by Imported() — is what must be checked.
func isDefaultLibrary(obj types.Object) bool {
	if pn, ok := obj.(*types.PkgName); ok {
		return isStdlibPath(pn.Imported().Path())
	}
	pkg := obj.Pkg()
	if pkg == nil {
		return true
	}
	return isStdlibPath(pkg.Path())
}

// isStdlibPath reports whether path looks like a standard-library import
// path, using the same heuristic cmd/go documents for distinguishing a
// module path (which must start with a domain name, and so always
// contains a dot in its first element) from the standard library (whose
// import paths never do).
func isStdlibPath(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// Encode converts tokens — which must be sorted by Range.StartOffset, with
// each Token confined to a single line (see SemanticTokens) — into the LSP
// semanticTokens wire format: 5 uint32s per token (deltaLine,
// deltaStartChar, length, tokenType, tokenModifiers), per the
// textDocument/semanticTokens encoding in the LSP specification. Line and
// character are both relative to the previous token (or to the start of
// the document for the first token), and character is counted in UTF-16
// code units to match the LSP wire protocol's position encoding — text
// must be the exact content the offsets in tokens were computed against.
func Encode(text []byte, tokens []Token) []uint32 {
	data := make([]uint32, 0, len(tokens)*5)
	var prevLine, prevChar uint32
	var line, char uint32
	pos := 0

	for _, t := range tokens {
		line, char, _ = advanceTo(text, pos, line, char, t.Range.StartOffset)
		length := utf16Length(text, t.Range.StartOffset, t.Range.EndOffset)

		deltaLine := line - prevLine
		deltaChar := char
		if deltaLine == 0 {
			deltaChar = char - prevChar
		}
		data = append(data, deltaLine, deltaChar, length, uint32(t.Kind), uint32(t.Modifiers))
		prevLine, prevChar = line, char

		// Advance the cursor past the token itself without re-walking its
		// bytes: a Token never spans a line break (see SemanticTokens), so
		// its end column is simply its start column plus its UTF-16 length.
		pos = t.Range.EndOffset
		char += length
	}
	return data
}

// advanceTo walks text forward from byte offset pos (currently at line,
// char) to byte offset target, tracking line count and UTF-16 column along
// the way, and returns the updated (line, char, pos = target).
func advanceTo(text []byte, pos int, line, char uint32, target int) (uint32, uint32, int) {
	for pos < target {
		r, size := utf8.DecodeRune(text[pos:])
		pos += size
		if r == '\n' {
			line++
			char = 0
		} else {
			char += utf16Units(r)
		}
	}
	return line, char, pos
}

// utf16Length returns the UTF-16 length of text[start:end], which must not
// contain a line break (guaranteed for a Token by SemanticTokens).
func utf16Length(text []byte, start, end int) uint32 {
	var n uint32
	for i := start; i < end; {
		r, size := utf8.DecodeRune(text[i:])
		n += utf16Units(r)
		i += size
	}
	return n
}

// utf16Units returns how many UTF-16 code units r encodes as: 2 for runes
// outside the Basic Multilingual Plane (which need a surrogate pair), 1
// otherwise.
func utf16Units(r rune) uint32 {
	if r > 0xffff {
		return 2
	}
	return 1
}
