package langfeat

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/sivchari/golance/internal/check"
)

// HintKind names a category of inlay hint. The string values match the
// keys gopls's "hints" initializationOption setting uses, so an editor
// config already written for gopls enables the same kinds in golance
// without changes.
type HintKind string

// HintKind values, one per hint InlayHints can produce.
const (
	AssignVariableTypes    HintKind = "assignVariableTypes"
	ParameterNames         HintKind = "parameterNames"
	RangeVariableTypes     HintKind = "rangeVariableTypes"
	CompositeLiteralFields HintKind = "compositeLiteralFields"
	CompositeLiteralTypes  HintKind = "compositeLiteralTypes"
	ConstantValues         HintKind = "constantValues"
	FunctionTypeParameters HintKind = "functionTypeParameters"
)

// AllHintKinds lists every HintKind InlayHints can produce.
var AllHintKinds = []HintKind{
	AssignVariableTypes,
	ParameterNames,
	RangeVariableTypes,
	CompositeLiteralFields,
	CompositeLiteralTypes,
	ConstantValues,
	FunctionTypeParameters,
}

// RenderKind is how an LSP client should render a hint. It mirrors the two
// non-zero values of protocol.InlayHintKind without this package depending
// on go.lsp.dev/protocol — see doc.go.
type RenderKind int

// RenderKind values.
const (
	RenderNone      RenderKind = iota // no particular kind (e.g. a constant's value)
	RenderType                        // a type annotation
	RenderParameter                   // a parameter name
)

// Hint is one inlay hint: a label to render at Offset (a byte offset from
// the start of the file), plus the rendering metadata the server layer
// needs to build a protocol.InlayHint.
type Hint struct {
	Offset       int
	Label        string
	Kind         HintKind
	Render       RenderKind
	PaddingLeft  bool
	PaddingRight bool
}

// ResolveHints expands a client's raw "hints" settings (keyed by the names
// in AllHintKinds, as in gopls's own setting) into a complete enabled-kind
// set. A kind is enabled unless raw explicitly sets it to false: a client
// that sends no settings at all, or omits a kind, gets every kind on by
// default (golance's more convenient out-of-the-box behavior compared to
// gopls, which defaults every kind off), while still letting an editor
// disable specific kinds one at a time.
func ResolveHints(raw map[string]bool) map[HintKind]bool {
	enabled := make(map[HintKind]bool, len(AllHintKinds))
	for _, k := range AllHintKinds {
		v, explicit := raw[string(k)]
		enabled[k] = !explicit || v
	}
	return enabled
}

// InlayHints returns every enabled inlay hint in file whose source range
// overlaps [startOffset, endOffset) (byte offsets from the start of the
// file), in source order.
func InlayHints(cp *check.CheckedPackage, file string, startOffset, endOffset int, enabled map[HintKind]bool) ([]Hint, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return nil, err
	}
	info := cp.Info()
	pkg := cp.Package()

	var hints []Hint
	ast.Inspect(astFile, func(n ast.Node) bool {
		if n == nil || !overlapsRange(tf, n, startOffset, endOffset) {
			return false
		}
		hints = append(hints, hintsForNode(info, pkg, tf, n, enabled)...)
		return true
	})
	sort.Slice(hints, func(i, j int) bool { return hints[i].Offset < hints[j].Offset })
	return hints, nil
}

// overlapsRange reports whether n's half-open source range [Pos, End)
// overlaps the half-open [start, end) requested. ast.Inspect returning
// false for a node whose range does not overlap prunes that whole subtree
// from the walk, the reason InlayHints can honor textDocument/inlayHint's
// Range without visiting a large file's entire AST.
func overlapsRange(tf *token.File, n ast.Node, start, end int) bool {
	return tf.Offset(n.Pos()) < end && tf.Offset(n.End()) > start
}

// hintsForNode returns the hints n itself contributes, for whichever of
// its enabled kinds apply to n's node type.
func hintsForNode(info *types.Info, pkg *types.Package, tf *token.File, n ast.Node, enabled map[HintKind]bool) []Hint {
	switch node := n.(type) {
	case *ast.AssignStmt:
		if enabled[AssignVariableTypes] && node.Tok == token.DEFINE {
			return assignVariableTypeHints(info, pkg, tf, node)
		}
	case *ast.RangeStmt:
		if enabled[RangeVariableTypes] {
			return rangeVariableTypeHints(info, pkg, tf, node)
		}
	case *ast.CallExpr:
		return callExprHints(info, pkg, tf, node, enabled)
	case *ast.CompositeLit:
		return compositeLitHints(info, pkg, tf, node, enabled)
	case *ast.GenDecl:
		if enabled[ConstantValues] && node.Tok == token.CONST {
			return constantValueHints(info, tf, node)
		}
	}
	return nil
}

// assignVariableTypeHints returns a type hint for each named (non-"_")
// identifier on the left-hand side of a ":=" short variable declaration.
func assignVariableTypeHints(info *types.Info, pkg *types.Package, tf *token.File, assign *ast.AssignStmt) []Hint {
	var hints []Hint
	for _, lhs := range assign.Lhs {
		if h, ok := variableTypeHint(info, pkg, tf, lhs, AssignVariableTypes); ok {
			hints = append(hints, h)
		}
	}
	return hints
}

// rangeVariableTypeHints returns a type hint for a "for k, v := range x"
// statement's key and/or value variable.
func rangeVariableTypeHints(info *types.Info, pkg *types.Package, tf *token.File, rng *ast.RangeStmt) []Hint {
	var hints []Hint
	if h, ok := variableTypeHint(info, pkg, tf, rng.Key, RangeVariableTypes); ok {
		hints = append(hints, h)
	}
	if h, ok := variableTypeHint(info, pkg, tf, rng.Value, RangeVariableTypes); ok {
		hints = append(hints, h)
	}
	return hints
}

// variableTypeHint returns a ": T" hint placed right after e, tagged with
// kind, for e's resolved type. It returns ok=false for a nil or "_"
// expression (the blank identifier has no useful type to annotate) or one
// whose type cannot be resolved.
func variableTypeHint(info *types.Info, pkg *types.Package, tf *token.File, e ast.Expr, kind HintKind) (Hint, bool) {
	if e == nil {
		return Hint{}, false
	}
	if id, ok := e.(*ast.Ident); ok && id.Name == "_" {
		return Hint{}, false
	}
	t := info.TypeOf(e)
	if t == nil {
		return Hint{}, false
	}
	return Hint{
		Offset: tf.Offset(e.End()),
		Label:  ": " + types.TypeString(t, types.RelativeTo(pkg)),
		Kind:   kind,
		Render: RenderType,
	}, true
}

// callExprHints returns call's enabled parameterNames and
// functionTypeParameters hints.
func callExprHints(info *types.Info, pkg *types.Package, tf *token.File, call *ast.CallExpr, enabled map[HintKind]bool) []Hint {
	var hints []Hint
	if enabled[ParameterNames] {
		hints = append(hints, parameterNameHints(info, tf, call)...)
	}
	if enabled[FunctionTypeParameters] {
		if h, ok := funcTypeParamHint(info, pkg, tf, call); ok {
			hints = append(hints, h)
		}
	}
	return hints
}

// parameterNameHints returns a "name:" hint before each of call's
// arguments whose corresponding parameter has a name, skipping an argument
// that is itself an identifier matching that parameter's name (redundant:
// gopls suppresses the same case). A variadic parameter's hint (on its
// first argument only) is labelled "name...:".
func parameterNameHints(info *types.Info, tf *token.File, call *ast.CallExpr) []Hint {
	t := info.TypeOf(call.Fun)
	if t == nil {
		return nil
	}
	sig, ok := t.Underlying().(*types.Signature)
	if !ok {
		return nil
	}
	params := sig.Params()
	var hints []Hint
	for i, arg := range call.Args {
		if i > params.Len()-1 {
			break
		}
		param := params.At(i)
		if param.Name() == "" {
			continue
		}
		if id, ok := arg.(*ast.Ident); ok && id.Name == param.Name() {
			continue
		}
		label := param.Name()
		if sig.Variadic() && i == params.Len()-1 {
			label += "..."
		}
		hints = append(hints, Hint{
			Offset:       tf.Offset(arg.Pos()),
			Label:        label + ":",
			Kind:         ParameterNames,
			Render:       RenderParameter,
			PaddingRight: true,
		})
	}
	return hints
}

// funcTypeParamHint returns a "[T, ...]" hint after call's function name,
// for a generic function call whose type arguments were inferred rather
// than written explicitly.
func funcTypeParamHint(info *types.Info, pkg *types.Package, tf *token.File, call *ast.CallExpr) (Hint, bool) {
	var id *ast.Ident
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	}
	if id == nil {
		return Hint{}, false
	}
	inst, ok := info.Instances[id]
	if !ok || inst.TypeArgs == nil || inst.TypeArgs.Len() == 0 {
		return Hint{}, false
	}
	args := make([]string, inst.TypeArgs.Len())
	for i := range args {
		args[i] = types.TypeString(inst.TypeArgs.At(i), types.RelativeTo(pkg))
	}
	return Hint{
		Offset: tf.Offset(id.End()),
		Label:  "[" + strings.Join(args, ", ") + "]",
		Kind:   FunctionTypeParameters,
		Render: RenderType,
	}, true
}

// compositeLitHints returns lit's enabled compositeLiteralFields and
// compositeLiteralTypes hints.
func compositeLitHints(info *types.Info, pkg *types.Package, tf *token.File, lit *ast.CompositeLit, enabled map[HintKind]bool) []Hint {
	var hints []Hint
	if enabled[CompositeLiteralFields] {
		hints = append(hints, compositeLiteralFieldHints(info, tf, lit)...)
	}
	if enabled[CompositeLiteralTypes] {
		if h, ok := compositeLiteralTypeHint(info, pkg, tf, lit); ok {
			hints = append(hints, h)
		}
	}
	return hints
}

// compositeLiteralFieldHints returns a "fieldName:" hint before each
// unkeyed element of a struct composite literal. A literal that already
// names its fields (key: value) needs no hint.
func compositeLiteralFieldHints(info *types.Info, tf *token.File, lit *ast.CompositeLit) []Hint {
	strct, ok := structOf(info.TypeOf(lit))
	if !ok {
		return nil
	}
	var hints []Hint
	for i, elt := range lit.Elts {
		if _, keyed := elt.(*ast.KeyValueExpr); keyed {
			continue
		}
		if i >= strct.NumFields() {
			break
		}
		hints = append(hints, Hint{
			Offset:       tf.Offset(elt.Pos()),
			Label:        strct.Field(i).Name() + ":",
			Kind:         CompositeLiteralFields,
			Render:       RenderParameter,
			PaddingRight: true,
		})
	}
	return hints
}

// structOf returns t's underlying *types.Struct, unwrapping a single level
// of pointer first (so &Point{...}'s literal still resolves).
func structOf(t types.Type) (*types.Struct, bool) {
	if t == nil {
		return nil, false
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	strct, ok := t.Underlying().(*types.Struct)
	return strct, ok
}

// compositeLiteralTypeHint returns a type-name hint at lit's opening brace,
// for a composite literal whose type is elided because it is implied by
// context (e.g. an element of a []Point or []*Point literal).
func compositeLiteralTypeHint(info *types.Info, pkg *types.Package, tf *token.File, lit *ast.CompositeLit) (Hint, bool) {
	if lit.Type != nil {
		return Hint{}, false
	}
	t := info.TypeOf(lit)
	if t == nil {
		return Hint{}, false
	}
	prefix := ""
	if p, ok := t.Underlying().(*types.Pointer); ok {
		prefix = "&"
		t = p.Elem()
	}
	return Hint{
		Offset: tf.Offset(lit.Lbrace),
		Label:  prefix + types.TypeString(t, types.RelativeTo(pkg)),
		Kind:   CompositeLiteralTypes,
		Render: RenderType,
	}, true
}

// constantValueHints returns a "= value[, value...]" hint after each spec
// in a const block whose constants' actual values are not already visible
// as literals in the source — most usefully, an iota-based spec.
func constantValueHints(info *types.Info, tf *token.File, decl *ast.GenDecl) []Hint {
	var hints []Hint
	for _, spec := range decl.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if h, ok := constantValueHint(info, tf, vs); ok {
			hints = append(hints, h)
		}
	}
	return hints
}

// constantValueHint returns vs's "= value[, value...]" hint, if any of its
// names' values are not already visible as literals in vs.Values.
func constantValueHint(info *types.Info, tf *token.File, vs *ast.ValueSpec) (Hint, bool) {
	showHint := len(vs.Values) == 0
	checkValues := len(vs.Names) == len(vs.Values)

	var values []string
	for i, name := range vs.Names {
		obj, ok := info.ObjectOf(name).(*types.Const)
		if !ok || obj.Val().Kind() == constant.Unknown {
			continue
		}
		if checkValues {
			if _, bad := vs.Values[i].(*ast.BadExpr); bad {
				continue
			}
			if !alreadyLiteral(vs.Values[i], obj.Val()) {
				showHint = true
			}
		}
		values = append(values, obj.Val().String())
	}
	if !showHint || len(values) == 0 {
		return Hint{}, false
	}
	return Hint{
		Offset:      tf.Offset(vs.End()),
		Label:       "= " + strings.Join(values, ", "),
		Kind:        ConstantValues,
		Render:      RenderNone,
		PaddingLeft: true,
	}, true
}

// alreadyLiteral reports whether value's source expression is a basic
// literal (or a boolean, whose "true"/"false" spelling is already its
// value) — the cases where printing the resolved value would just repeat
// what the source already shows.
func alreadyLiteral(expr ast.Expr, value constant.Value) bool {
	if _, ok := expr.(*ast.BasicLit); ok {
		return true
	}
	return value.Kind() == constant.Bool
}
