package langfeat

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sivchari/golance/internal/check"
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
	// AdditionalTextEdits are edits applied alongside accepting this item,
	// disjoint from wherever the client inserts Label itself — currently
	// only ever the import-block insertion an unimported-package candidate
	// needs (see UnimportedPackageItems/UnimportedMemberItems). nil for
	// every other candidate.
	AdditionalTextEdits []Edit
}

// Completion returns completion candidates for the cursor at offset (a
// byte offset from the start of file) against text, which must be the same
// content cp was checked against (see check.CheckedPackage.FileText) — it
// finds the identifier prefix being typed there, then resolves the
// surrounding context (selector, package member, or lexical scope) against
// cp's already-checked types.
//
// A dangling selector ("x." with nothing typed after the dot yet) usually
// still resolves: parser.AllErrors keeps the SelectorExpr in the partial
// AST, and go/types still records x's static type even though the
// selection itself is invalid. If x's type genuinely cannot be resolved
// (e.g. x itself sits inside a still-more-broken expression), Completion
// falls back to lexical scope candidates instead — recovering that case
// too would need a per-expression types.CheckExpr scratch check, a known
// v0.1 limitation (see TODO below).
func Completion(cp *check.CheckedPackage, text []byte, file string, offset int) ([]CompletionItem, error) {
	// text and offset are expected to already agree (the caller should have
	// derived offset from this same text), but clamp defensively anyway:
	// this is a public entry point, and text[prefixStart:offset] below must
	// never run out of bounds even against a stale offset.
	offset = min(max(offset, 0), len(text))
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

// UnimportedContext describes what unimported-package lookup, if any,
// applies to the same cursor position a Completion call was made against —
// a second, cheap pass over the same AST (Completion's own path is not
// exposed to callers), for the server layer to resolve candidates against
// its own workspace/graph state (Completion itself never reads the graph)
// and turn into CompletionItems via UnimportedPackageItems/
// UnimportedMemberItems.
type UnimportedContext struct {
	// Prefix is the identifier prefix already typed at the cursor — the
	// package name being typed for a Selector == "" (shape 1) context, or
	// the member name prefix for a Selector != "" (shape 2) one.
	Prefix string
	// Selector is the base identifier's name in a "Selector.Prefix"
	// qualified-selector position whose base does not already resolve (to
	// an imported package or a value) — a shape-2 candidate. Empty for a
	// bare lexical-position prefix instead (shape 1).
	Selector string
}

// Unimported reports the UnimportedContext for the cursor at offset in
// text (see Completion for the shared coordinate system), or ok=false if
// no unimported-package lookup applies there: a shape-2 selector whose
// base already resolves (nothing to add), or a shape-1 lexical position
// with an empty prefix — mirroring gopls's own "don't suggest unimported
// packages if we have absolutely nothing to go on" cutoff, since scoring
// every package in the workspace against an empty prefix is all cost and
// no signal.
func Unimported(cp *check.CheckedPackage, text []byte, file string, offset int) (UnimportedContext, bool) {
	offset = min(max(offset, 0), len(text))
	prefixStart := scanIdentBack(text, offset)
	prefix := string(text[prefixStart:offset])

	astFile, ctxPos, _, err := locate(cp, file, prefixStart)
	if err != nil {
		return UnimportedContext{}, false
	}
	path, _ := astutil.PathEnclosingInterval(astFile, ctxPos, ctxPos)

	if sel := enclosingSelector(path); sel != nil {
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return UnimportedContext{}, false
		}
		if _, ok := cp.Info().ObjectOf(id).(*types.PkgName); ok {
			return UnimportedContext{}, false // already imported; ordinary selectorCompletions handles it
		}
		if cp.Info().TypeOf(sel.X) != nil {
			return UnimportedContext{}, false // resolves to a value; ordinary member completion handles it
		}
		return UnimportedContext{Selector: id.Name, Prefix: prefix}, true
	}

	if prefix == "" {
		return UnimportedContext{}, false
	}
	return UnimportedContext{Prefix: prefix}, true
}

// UnimportedPackageCandidate names a graph-known package that could satisfy
// an unimported-package completion: its declared name (e.g. "strings",
// read from its package clause — can differ from its import path's last
// segment) and import path.
type UnimportedPackageCandidate struct {
	Name       string
	ImportPath string
}

// UnimportedPackageItems returns one KindPackage CompletionItem per
// candidate (shape 1: the user is typing a package name itself, not yet
// imported), ranked below every in-scope candidate via SortText (see the
// rank* constants) — each carrying the AdditionalTextEdits that import
// candidate's path into file's current content text. A candidate whose
// import edit cannot be computed (e.g. text fails to parse) is silently
// skipped rather than failing the whole completion request.
func UnimportedPackageItems(file string, text []byte, prefix string, candidates []UnimportedPackageCandidate) []CompletionItem {
	items := make([]CompletionItem, 0, len(candidates))
	for _, c := range candidates {
		edit, err := importInsertEdit(file, text, c.Name, c.ImportPath)
		if err != nil {
			continue
		}
		items = append(items, CompletionItem{
			Label:               c.Name,
			Kind:                KindPackage,
			Detail:              fmt.Sprintf("package (from %q)", c.ImportPath),
			AdditionalTextEdits: []Edit{edit},
		})
	}
	return filterAndRankBase(items, prefix, rankUnimported, rankUnimportedFuzzy)
}

// UnimportedMemberItems returns candidate's exported member
// CompletionItems (shape 2: "pkg.Prefix" where pkg names candidate but is
// not yet imported), matching prefix — reusing the same
// packageMemberItems/kindForObject machinery selectorCompletions uses for
// an already-imported package, so Kind/Detail formatting is identical.
// Every returned item carries the same AdditionalTextEdits importing
// candidate's path; pkg is candidate's already-decoded *types.Package (the
// caller resolves this from export data — see internal/typecheck — since
// Unimported itself never reads the graph).
func UnimportedMemberItems(file string, text []byte, prefix string, candidate UnimportedPackageCandidate, pkg *types.Package) []CompletionItem {
	edit, err := importInsertEdit(file, text, candidate.Name, candidate.ImportPath)
	if err != nil {
		return nil
	}
	items := packageMemberItems(pkg)
	for i := range items {
		items[i].AdditionalTextEdits = []Edit{edit}
	}
	return filterAndRankBase(items, prefix, rankUnimported, rankUnimportedFuzzy)
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
	return filterAndRank(memberItems(cp.Package(), xType), prefix)
}

// memberItems collects t's method set (value and pointer receivers) and,
// if t is (or points to) a struct, its immediate fields. pkg is the
// package being edited, used to qualify a member's type only when it comes
// from a different package.
func memberItems(pkg *types.Package, t types.Type) []CompletionItem {
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
			Detail: types.ObjectString(obj, qualifier(pkg)),
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
				Detail: types.ObjectString(f, qualifier(pkg)),
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
			Detail: types.ObjectString(obj, qualifier(pkg)),
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
				Detail: types.ObjectString(obj, qualifier(cp.Package())),
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

// Rank prefixes filterAndRankBase's SortText values sort by: in-scope
// candidates (rankExact/rankFuzzy) always precede unimported ones
// (rankUnimported), matching gopls's unimportedScore, which always scores
// below any in-scope candidate — see UnimportedPackageItems/
// UnimportedMemberItems.
const (
	rankExact           = "0"
	rankFuzzy           = "1"
	rankUnimported      = "2"
	rankUnimportedFuzzy = "3"
)

// filterAndRank keeps only items whose Label matches prefix (case-sensitive
// prefix match ranked above case-insensitive), sets each survivor's
// SortText accordingly, and returns them sorted by rank then label.
func filterAndRank(items []CompletionItem, prefix string) []CompletionItem {
	return filterAndRankBase(items, prefix, rankExact, rankFuzzy)
}

// filterAndRankBase is filterAndRank generalized over the rank prefixes
// used for an exact-prefix versus a case-insensitive match, so
// UnimportedPackageItems/UnimportedMemberItems can rank their own matches
// below every in-scope item (see the rank* constants) while reusing the
// same matching logic.
func filterAndRankBase(items []CompletionItem, prefix, exactBase, fuzzyBase string) []CompletionItem {
	lowerPrefix := strings.ToLower(prefix)
	out := make([]CompletionItem, 0, len(items))
	for _, it := range items {
		switch {
		case prefix == "" || strings.HasPrefix(it.Label, prefix):
			it.SortText = exactBase + it.Label
		case strings.HasPrefix(strings.ToLower(it.Label), lowerPrefix):
			it.SortText = fuzzyBase + it.Label
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
// identifier rune. offset is clamped to [0, len(text)] first: callers'
// offsets are validated against one overlay read, but ResolveCompletionDoc
// passes its own offset straight through without re-checking it against the
// text (read separately, and possibly later) passed in here.
func scanIdentBack(text []byte, offset int) int {
	i := min(max(offset, 0), len(text))
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
