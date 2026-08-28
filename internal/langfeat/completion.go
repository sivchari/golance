package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/overlay"
	"golang.org/x/tools/go/ast/astutil"
)

// CompletionKind categorizes a CompletionItem, independent of any LSP
// protocol type.
type CompletionKind int

// Kinds a CompletionItem can have.
const (
	KindText CompletionKind = iota
	KindFunc
	KindMethod
	KindField
	KindVar
	KindConst
	KindType
	KindPackage
)

// CompletionItem is one completion candidate.
type CompletionItem struct {
	Label    string
	Kind     CompletionKind
	Detail   string
	SortText string
}

// Completion returns completion candidates for the cursor at offset (a
// byte offset from the start of file). It reads file's current content
// through reader to find the identifier prefix being typed, then resolves
// the surrounding context (selector, package member, or lexical scope)
// against cp's already-checked types.
//
// A dangling selector ("x." with nothing typed after the dot yet) usually
// still resolves: parser.AllErrors keeps the SelectorExpr in the partial
// AST, and go/types still records x's static type even though the
// selection itself is invalid. If x's type genuinely cannot be resolved
// (e.g. x itself sits inside a still-more-broken expression), Completion
// falls back to lexical scope candidates instead — recovering that case
// too would need a per-expression types.CheckExpr scratch check, a known
// v0.1 limitation (see TODO below).
func Completion(cp *check.CheckedPackage, reader overlay.FileReader, file string, offset int) ([]CompletionItem, error) {
	text, err := reader.ReadFile(file)
	if err != nil {
		return nil, err
	}
	prefixStart := scanIdentBack(text, offset)
	prefix := string(text[prefixStart:offset])

	astFile, ctxPos, _, err := locate(cp, file, prefixStart)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, ctxPos, ctxPos)

	if sel := enclosingSelector(path); sel != nil {
		return selectorCompletions(cp, sel, prefix), nil
	}
	return lexicalCompletions(cp, ctxPos, prefix), nil
}

// MergeUnimported concatenates candidates (typically resolved from the
// xref symbol-name index by the server layer, for symbols not yet
// imported) after items. It performs no deduplication or re-ranking: the
// server layer owns how unimported candidates are sourced and ordered.
func MergeUnimported(items, candidates []CompletionItem) []CompletionItem {
	return append(items, candidates...)
}

// enclosingSelector returns the nearest *ast.SelectorExpr in path, or nil
// if path contains none.
func enclosingSelector(path []ast.Node) *ast.SelectorExpr {
	for _, n := range path {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			return sel
		}
	}
	return nil
}

// selectorCompletions handles "x.<prefix>" completion: package member
// completion if x names an imported package, otherwise x's method set
// (value and pointer receivers) plus its struct fields.
func selectorCompletions(cp *check.CheckedPackage, sel *ast.SelectorExpr, prefix string) []CompletionItem {
	if id, ok := sel.X.(*ast.Ident); ok {
		if pn, ok := cp.Info().ObjectOf(id).(*types.PkgName); ok {
			return filterAndRank(packageMemberItems(pn.Imported()), prefix)
		}
	}

	xType := cp.Info().TypeOf(sel.X)
	if xType == nil {
		// TODO(v0.1): recover this case with a types.CheckExpr scratch check
		// of sel.X against the last-good package scope, instead of giving up
		// on selector completion entirely.
		return nil
	}
	return filterAndRank(memberItems(xType), prefix)
}

// memberItems collects t's method set (value and pointer receivers) and,
// if t is (or points to) a struct, its immediate fields.
func memberItems(t types.Type) []CompletionItem {
	seen := make(map[string]bool)
	var items []CompletionItem
	addMethod := func(sel *types.Selection) {
		obj := sel.Obj()
		if seen[obj.Name()] {
			return
		}
		seen[obj.Name()] = true
		items = append(items, CompletionItem{
			Label:  obj.Name(),
			Kind:   KindMethod,
			Detail: types.ObjectString(obj, nil),
		})
	}

	ms := types.NewMethodSet(t)
	for i := range ms.Len() {
		addMethod(ms.At(i))
	}
	if _, isPtr := t.(*types.Pointer); !isPtr {
		pms := types.NewMethodSet(types.NewPointer(t))
		for i := range pms.Len() {
			addMethod(pms.At(i))
		}
	}

	if st := underlyingStruct(t); st != nil {
		for i := range st.NumFields() {
			f := st.Field(i)
			if seen[f.Name()] {
				continue
			}
			seen[f.Name()] = true
			items = append(items, CompletionItem{
				Label:  f.Name(),
				Kind:   KindField,
				Detail: types.ObjectString(f, nil),
			})
		}
	}
	return items
}

// underlyingStruct returns t's underlying *types.Struct, unwrapping a
// single pointer indirection first. It returns nil if t is not (or does
// not point to) a struct.
func underlyingStruct(t types.Type) *types.Struct {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	st, _ := t.Underlying().(*types.Struct)
	return st
}

// packageMemberItems returns pkg's exported package-level members.
func packageMemberItems(pkg *types.Package) []CompletionItem {
	scope := pkg.Scope()
	items := make([]CompletionItem, 0, len(scope.Names()))
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		items = append(items, CompletionItem{
			Label:  name,
			Kind:   kindForObject(obj),
			Detail: types.ObjectString(obj, types.RelativeTo(pkg)),
		})
	}
	return items
}

// lexicalCompletions returns every symbol visible at pos: the innermost
// block scope enclosing pos, walked outward through its enclosing scopes
// (function, package, and finally the Universe scope of predeclared
// identifiers).
func lexicalCompletions(cp *check.CheckedPackage, pos token.Pos, prefix string) []CompletionItem {
	scope := cp.Package().Scope().Innermost(pos)
	if scope == nil {
		scope = cp.Package().Scope()
	}
	seen := make(map[string]bool)
	var items []CompletionItem
	for s := scope; s != nil; s = s.Parent() {
		for _, name := range s.Names() {
			if seen[name] {
				continue
			}
			seen[name] = true
			obj := s.Lookup(name)
			items = append(items, CompletionItem{
				Label:  name,
				Kind:   kindForObject(obj),
				Detail: types.ObjectString(obj, types.RelativeTo(cp.Package())),
			})
		}
	}
	return filterAndRank(items, prefix)
}

// kindForObject maps a types.Object to its CompletionKind.
func kindForObject(obj types.Object) CompletionKind {
	switch o := obj.(type) {
	case *types.Func:
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			return KindMethod
		}
		return KindFunc
	case *types.Var:
		if o.IsField() {
			return KindField
		}
		return KindVar
	case *types.Const:
		return KindConst
	case *types.TypeName:
		return KindType
	case *types.PkgName:
		return KindPackage
	default:
		return KindText
	}
}

// filterAndRank keeps only items whose Label matches prefix (case-sensitive
// prefix match ranked above case-insensitive), sets each survivor's
// SortText accordingly, and returns them sorted by rank then label.
func filterAndRank(items []CompletionItem, prefix string) []CompletionItem {
	lowerPrefix := strings.ToLower(prefix)
	out := make([]CompletionItem, 0, len(items))
	for _, it := range items {
		switch {
		case prefix == "" || strings.HasPrefix(it.Label, prefix):
			it.SortText = "0" + it.Label
		case strings.HasPrefix(strings.ToLower(it.Label), lowerPrefix):
			it.SortText = "1" + it.Label
		default:
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortText < out[j].SortText })
	return out
}

// scanIdentBack returns the byte offset of the start of the identifier
// ending at offset in text, or offset itself if text[offset-1] is not an
// identifier rune.
func scanIdentBack(text []byte, offset int) int {
	i := offset
	for i > 0 {
		r, size := utf8.DecodeLastRune(text[:i])
		if !isIdentRune(r) {
			break
		}
		i -= size
	}
	return i
}

func isIdentRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
