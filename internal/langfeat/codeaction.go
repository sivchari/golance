package langfeat

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"

	"golang.org/x/tools/go/ast/astutil"
)

// ActionKind categorizes a CodeAction, independent of any LSP protocol
// type.
type ActionKind int

// Kinds a CodeAction can have.
const (
	ActionQuickFix ActionKind = iota
	ActionSourceOrganizeImports
)

// CodeAction is one suggested fix or source action: a title for display,
// its Kind, and the Edits (in this package's byte-offset coordinate
// system) it applies to the file the query was run against.
type CodeAction struct {
	Title string
	Kind  ActionKind
	Edits []Edit
}

// Edit is a single text replacement, in this package's byte-offset
// coordinate system.
type Edit struct {
	Range   Range
	NewText string
}

// OrganizeImportsAction returns the source.organizeImports CodeAction for
// file's current content text, or ok=false if OrganizeImports makes no
// change (already organized) or cannot process text (e.g. a syntax error).
func OrganizeImportsAction(file string, text []byte) (CodeAction, bool, error) {
	out, err := OrganizeImports(file, text)
	if err != nil {
		return CodeAction{}, false, err
	}
	if bytes.Equal(text, out) {
		return CodeAction{}, false, nil
	}
	return CodeAction{
		Title: "Organize imports",
		Kind:  ActionSourceOrganizeImports,
		Edits: []Edit{organizeImportsEdit(text, out)},
	}, true, nil
}

// organizeImportsEdit returns the single edit turning text into out,
// confined to the smallest contiguous span of changed lines (the
// common-prefix/common-suffix line diff between the two) instead of
// replacing the whole file — matching gopls's source.organizeImports, which
// diffs only the changed region (see computeFixEdits in gopls's
// internal/golang/format.go).
func organizeImportsEdit(text, out []byte) Edit {
	fromLines := bytes.Split(text, []byte{'\n'})
	toLines := bytes.Split(out, []byte{'\n'})
	prefix, suffix := commonPrefixSuffixLines(fromLines, toLines)

	fromOffs := lineStartOffsets(fromLines)
	toOffs := lineStartOffsets(toLines)
	start := fromOffs[prefix]
	end := fromOffs[len(fromLines)-suffix]
	newText := string(out[toOffs[prefix]:toOffs[len(toLines)-suffix]])

	return Edit{Range: Range{StartOffset: start, EndOffset: end}, NewText: newText}
}

// commonPrefixSuffixLines returns the number of leading and
// (non-overlapping) trailing lines a and b have in common.
func commonPrefixSuffixLines(a, b [][]byte) (prefix, suffix int) {
	n := min(len(a), len(b))
	for prefix < n && bytes.Equal(a[prefix], b[prefix]) {
		prefix++
	}
	maxSuffix := n - prefix
	for suffix < maxSuffix && bytes.Equal(a[len(a)-1-suffix], b[len(b)-1-suffix]) {
		suffix++
	}
	return prefix, suffix
}

// lineStartOffsets returns, for each index in [0, len(lines)], the byte
// offset of the start of lines[idx] in bytes.Join(lines, "\n") —
// lineStartOffsets(lines)[len(lines)] is that joined content's total
// length.
func lineStartOffsets(lines [][]byte) []int {
	offs := make([]int, len(lines)+1)
	pos := 0
	for i, l := range lines {
		offs[i] = pos
		pos += len(l)
		if i < len(lines)-1 {
			pos++ // the '\n' separating this line from the next
		}
	}
	offs[len(lines)] = pos
	return offs
}

// parseSource parses text as a standalone Go file. Every CodeAction in
// this file works from source text and positions alone, never from
// type-check results, so a fresh parse is all it needs — and, unlike
// reusing a check.CheckedPackage's cached AST, this one is never shared
// with anything else that might read it concurrently.
func parseSource(file string, text []byte) (*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, file, text, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("langfeat: parse %s: %w", file, err)
	}
	return astFile, fset, nil
}

// posForFileOffset converts a byte offset in tf's file to a token.Pos, or
// ok=false if offset falls outside the file.
func posForFileOffset(tf *token.File, offset int) (token.Pos, bool) {
	if offset < 0 || offset > tf.Size() {
		return token.NoPos, false
	}
	return tf.Pos(offset), true
}

// UnusedImportFix returns the CodeAction removing the import declaration
// at offset (a byte offset into file's current content text — an
// "... imported and not used" diagnostic's position), or ok=false if
// offset does not resolve to an import spec.
func UnusedImportFix(file string, text []byte, offset int) (CodeAction, bool, error) {
	astFile, fset, err := parseSource(file, text)
	if err != nil {
		return CodeAction{}, false, err
	}
	tf := fset.File(astFile.Pos())
	pos, ok := posForFileOffset(tf, offset)
	if !ok {
		return CodeAction{}, false, nil
	}
	decl, spec := importSpecAt(astFile, pos)
	if decl == nil {
		return CodeAction{}, false, nil
	}
	start, end := importRemovalRange(tf, text, decl, spec)
	return CodeAction{
		Title: fmt.Sprintf("Remove import %s", spec.Path.Value),
		Kind:  ActionQuickFix,
		Edits: []Edit{{Range: Range{StartOffset: start, EndOffset: end}, NewText: ""}},
	}, true, nil
}

// importSpecAt returns the import declaration and spec containing pos, or
// (nil, nil) if pos is not within any import spec.
func importSpecAt(f *ast.File, pos token.Pos) (*ast.GenDecl, *ast.ImportSpec) {
	for _, d := range f.Decls {
		decl, ok := d.(*ast.GenDecl)
		if !ok || decl.Tok != token.IMPORT {
			continue
		}
		for _, s := range decl.Specs {
			spec, ok := s.(*ast.ImportSpec)
			if ok && pos >= spec.Pos() && pos < spec.End() {
				return decl, spec
			}
		}
	}
	return nil, nil
}

// importRemovalRange returns the [start, end) byte-offset span to delete
// for removing spec from decl: the whole declaration (including its
// "import" keyword) if spec is its only one, otherwise just spec's own
// line. Either way the span extends through the line's trailing newline,
// so removing it leaves no blank line behind. Removing the whole
// declaration also consumes one immediately following blank line, since
// gofmt-style source conventionally surrounds an import block with one —
// leaving it would otherwise turn that single blank line into two.
func importRemovalRange(tf *token.File, text []byte, decl *ast.GenDecl, spec *ast.ImportSpec) (start, end int) {
	if len(decl.Specs) == 1 {
		start, end = lineSpan(tf, tf.Position(decl.Pos()).Line, tf.Position(decl.End()).Line)
		return start, consumeBlankLine(text, end)
	}
	return lineSpan(tf, tf.Position(spec.Pos()).Line, tf.Position(spec.End()).Line)
}

// consumeBlankLine returns end+1 if text's line starting at end is blank
// (just a newline), otherwise end unchanged.
func consumeBlankLine(text []byte, end int) int {
	if end < len(text) && text[end] == '\n' {
		return end + 1
	}
	return end
}

// lineSpan returns the [start, end) byte-offset span covering startLine
// through endLine of tf's file, inclusive, through the start of the
// following line (i.e. including endLine's trailing newline) — or through
// the end of the file if endLine is its last line.
func lineSpan(tf *token.File, startLine, endLine int) (start, end int) {
	start = tf.Offset(tf.LineStart(startLine))
	if endLine+1 <= tf.LineCount() {
		end = tf.Offset(tf.LineStart(endLine + 1))
	} else {
		end = tf.Size()
	}
	return start, end
}

// UnusedVarFix returns the CodeAction resolving a "declared and not used:
// Name" diagnostic at offset (a byte offset into file's current content
// text — the declaration's own position): it blanks the declared
// identifier, and — for a short variable declaration whose only declared
// name this is — also turns its ":=" into "=" so the statement stays valid
// once it declares nothing new. ok is false if offset does not resolve to
// a var declaration this can fix; only plain "var" and ":=" declarations
// are handled (e.g. a range clause's loop variable is not).
func UnusedVarFix(file string, text []byte, offset int) (CodeAction, bool, error) {
	astFile, fset, err := parseSource(file, text)
	if err != nil {
		return CodeAction{}, false, err
	}
	tf := fset.File(astFile.Pos())
	pos, ok := posForFileOffset(tf, offset)
	if !ok {
		return CodeAction{}, false, nil
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil || len(path) < 2 {
		return CodeAction{}, false, nil
	}

	edits, ok := blankDeclEdits(tf, path[1], id)
	if !ok {
		return CodeAction{}, false, nil
	}
	return CodeAction{
		Title: fmt.Sprintf("Remove unused variable %s", id.Name),
		Kind:  ActionQuickFix,
		Edits: edits,
	}, true, nil
}

// blankDeclEdits returns the edits blanking id's declaration, given parent
// — id's immediate enclosing node. ok is false unless parent is a
// *ast.ValueSpec ("var") or a *ast.AssignStmt (":=") that actually
// declares id.
func blankDeclEdits(tf *token.File, parent ast.Node, id *ast.Ident) ([]Edit, bool) {
	switch p := parent.(type) {
	case *ast.ValueSpec:
		if !containsIdent(p.Names, id) {
			return nil, false
		}
		return []Edit{identEdit(tf, id)}, true
	case *ast.AssignStmt:
		if p.Tok != token.DEFINE || !containsExprIdent(p.Lhs, id) {
			return nil, false
		}
		edits := []Edit{identEdit(tf, id)}
		if countNonBlank(p.Lhs) == 1 {
			tokStart := tf.Offset(p.TokPos)
			edits = append(edits, Edit{
				Range:   Range{StartOffset: tokStart, EndOffset: tokStart + len(":=")},
				NewText: "=",
			})
		}
		return edits, true
	default:
		return nil, false
	}
}

func identEdit(tf *token.File, id *ast.Ident) Edit {
	return Edit{
		Range:   Range{StartOffset: tf.Offset(id.Pos()), EndOffset: tf.Offset(id.End())},
		NewText: "_",
	}
}

func containsIdent(names []*ast.Ident, id *ast.Ident) bool {
	for _, n := range names {
		if n == id {
			return true
		}
	}
	return false
}

func containsExprIdent(exprs []ast.Expr, id *ast.Ident) bool {
	for _, e := range exprs {
		if e == id {
			return true
		}
	}
	return false
}

// countNonBlank returns how many of exprs are an identifier other than
// "_".
func countNonBlank(exprs []ast.Expr) int {
	n := 0
	for _, e := range exprs {
		if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
			n++
		}
	}
	return n
}

// ImportCandidate identifies one exported package-level symbol that could
// resolve an "undefined: Name" diagnostic: PackageName is its declaring
// package's actual name (read from its package clause, used to qualify
// the identifier — it can differ from its import path's last component),
// ImportPath is the import path to add.
type ImportCandidate struct {
	PackageName string
	ImportPath  string
}

// AddImportFix returns one CodeAction per candidate, each qualifying the
// identifier spanning [offset, offset+len(name)) in file's current content
// text with the candidate's package name and adding an import for its
// import path, for an "undefined: Name" diagnostic at that position.
func AddImportFix(file string, text []byte, offset int, name string, candidates []ImportCandidate) ([]CodeAction, error) {
	if offset < 0 || offset+len(name) > len(text) {
		return nil, fmt.Errorf("langfeat: offset %d out of range for %s", offset, file)
	}
	actions := make([]CodeAction, 0, len(candidates))
	for _, c := range candidates {
		action, err := addImportAction(file, text, offset, name, c)
		if err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	return actions, nil
}

// addImportAction qualifies the identifier at offset with c.PackageName
// and adds an import for c.ImportPath, working from a disposable parse of
// the already-qualified text rather than mutating any AST shared with a
// cached CheckedPackage. The result is reformatted through OrganizeImports
// so the added import is sorted and grouped the same way
// source.organizeImports would leave it.
func addImportAction(file string, text []byte, offset int, name string, c ImportCandidate) (CodeAction, error) {
	qualified := make([]byte, 0, len(text)+len(c.PackageName)+1)
	qualified = append(qualified, text[:offset]...)
	qualified = append(qualified, c.PackageName...)
	qualified = append(qualified, '.')
	qualified = append(qualified, text[offset:]...)

	astFile, fset, err := parseSource(file, qualified)
	if err != nil {
		return CodeAction{}, fmt.Errorf("langfeat: qualify %s with %s: %w", name, c.PackageName, err)
	}
	astutil.AddNamedImport(fset, astFile, "", c.ImportPath)

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, astFile); err != nil {
		return CodeAction{}, fmt.Errorf("langfeat: print %s: %w", file, err)
	}
	out, err := OrganizeImports(file, buf.Bytes())
	if err != nil {
		return CodeAction{}, fmt.Errorf("langfeat: organize imports for %s: %w", file, err)
	}

	return CodeAction{
		Title: fmt.Sprintf("Import %q as %s", c.ImportPath, c.PackageName),
		Kind:  ActionQuickFix,
		Edits: []Edit{{Range: Range{StartOffset: 0, EndOffset: len(text)}, NewText: string(out)}},
	}, nil
}
