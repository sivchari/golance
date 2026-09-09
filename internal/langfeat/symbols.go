package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/sivchari/golance/internal/check"
)

// SymbolKind categorizes a Symbol, independent of any LSP protocol type.
type SymbolKind int

// Kinds a Symbol can have.
const (
	SymbolFunc SymbolKind = iota
	SymbolMethod
	SymbolType
	SymbolInterface
	SymbolVar
	SymbolConst
	SymbolField
)

// Symbol is one declaration in a file's outline. Methods are nested under
// the Symbol for their receiver type when that type is declared in the
// same file; otherwise they appear at the top level alongside it. A
// struct's fields and an interface's method set (and embedded types) are
// always nested under their declaring type's Symbol, matching gopls's own
// documentSymbol outline (verified with `gopls symbols`).
type Symbol struct {
	Name     string
	Kind     SymbolKind
	Range    Range
	Children []Symbol
}

// DocumentSymbols returns file's declarations as a hierarchical outline:
// top-level types, funcs, vars, and consts, with methods nested under
// their receiver type and struct fields/interface members nested under
// their declaring type.
func DocumentSymbols(cp *check.CheckedPackage, file string) ([]Symbol, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return nil, err
	}

	var top []Symbol
	typeIndex := make(map[string]int)
	for _, decl := range astFile.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			top = append(top, Symbol{
				Name:     ts.Name.Name,
				Kind:     typeSpecKind(ts),
				Range:    rangeOf(tf, ts.Pos(), ts.End()),
				Children: typeSpecChildren(tf, ts),
			})
			typeIndex[ts.Name.Name] = len(top) - 1
		}
	}

	for _, decl := range astFile.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			top = appendFuncSymbol(top, typeIndex, tf, d)
		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				continue
			}
			top = appendValueSymbols(top, tf, d)
		}
	}
	return top, nil
}

// appendFuncSymbol appends d's Symbol to top: nested under its receiver
// type's Symbol (found via typeIndex) if it has one, otherwise at the top
// level.
func appendFuncSymbol(top []Symbol, typeIndex map[string]int, tf *token.File, d *ast.FuncDecl) []Symbol {
	if d.Recv == nil {
		return append(top, Symbol{
			Name:  d.Name.Name,
			Kind:  SymbolFunc,
			Range: rangeOf(tf, d.Pos(), d.End()),
		})
	}
	m := Symbol{Name: d.Name.Name, Kind: SymbolMethod, Range: rangeOf(tf, d.Pos(), d.End())}
	if idx, ok := typeIndex[receiverTypeName(d.Recv)]; ok {
		top[idx].Children = append(top[idx].Children, m)
		return top
	}
	return append(top, m)
}

// appendValueSymbols appends one Symbol per declared name in d (a var or
// const GenDecl) to top.
func appendValueSymbols(top []Symbol, tf *token.File, d *ast.GenDecl) []Symbol {
	kind := SymbolVar
	if d.Tok == token.CONST {
		kind = SymbolConst
	}
	for _, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, name := range vs.Names {
			if name.Name == "_" {
				continue
			}
			top = append(top, Symbol{
				Name:  name.Name,
				Kind:  kind,
				Range: rangeOf(tf, name.Pos(), name.End()),
			})
		}
	}
	return top
}

// receiverTypeName returns the unqualified type name a method's receiver
// binds to, unwrapping a pointer and any generic type parameters. It
// returns "" if recv does not describe a single named-type receiver.
func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// typeSpecKind returns SymbolInterface for an interface declaration,
// SymbolType for everything else (struct or a plain defined type such as
// `type Level int`) -- the same struct-vs-interface granularity
// workspaceSymbolKind (internal/server/kinds.go) already carries via the
// facts index's index.KindType/index.KindInterface, kept consistent here
// rather than introducing a finer bucket (gopls itself further
// distinguishes a plain defined type as "Class") the facts index has no
// equivalent for.
func typeSpecKind(ts *ast.TypeSpec) SymbolKind {
	if _, ok := ts.Type.(*ast.InterfaceType); ok {
		return SymbolInterface
	}
	return SymbolType
}

// typeSpecChildren returns ts's nested outline entries: a struct's fields,
// or an interface's method set and embedded types -- verified against
// `gopls symbols` to nest exactly these, and only these (gopls does not
// nest same-file methods under their receiver type; see appendFuncSymbol's
// own, pre-existing and separately tested nesting for those). nil for a
// type whose underlying type is neither, e.g. `type Level int`.
func typeSpecChildren(tf *token.File, ts *ast.TypeSpec) []Symbol {
	switch t := ts.Type.(type) {
	case *ast.StructType:
		return fieldListChildren(tf, t.Fields, SymbolField)
	case *ast.InterfaceType:
		return fieldListChildren(tf, t.Methods, SymbolMethod)
	default:
		return nil
	}
}

// fieldListChildren returns one Symbol per entry in fields: a named entry
// (a struct field, or an interface method spec) becomes namedKind once per
// name in Names; an unnamed entry -- an embedded struct field, an embedded
// interface, or a generic interface's type-set element (e.g. `~int |
// ~int32`) -- always becomes SymbolField, named per embeddedElementName.
func fieldListChildren(tf *token.File, fields *ast.FieldList, namedKind SymbolKind) []Symbol {
	if fields == nil {
		return nil
	}
	var out []Symbol
	for _, f := range fields.List {
		if len(f.Names) == 0 {
			out = append(out, Symbol{
				Name:  embeddedElementName(f.Type),
				Kind:  SymbolField,
				Range: rangeOf(tf, f.Type.Pos(), f.Type.End()),
			})
			continue
		}
		for _, name := range f.Names {
			out = append(out, Symbol{
				Name:  name.Name,
				Kind:  namedKind,
				Range: rangeOf(tf, name.Pos(), name.End()),
			})
		}
	}
	return out
}

// embeddedElementName names an unnamed field/interface-method-spec entry:
// per the Go spec, an embedded field or embedded interface's name is its
// type name's own unqualified identifier -- unwrapping a pointer and any
// generic instantiation, and dropping a package qualifier -- exactly like
// `gopls symbols` labels one ("baseServer Field", "Reader Field" for an
// embedded io.Reader). expr that names no single type at all (a generic
// interface's type-set element, e.g. `~int | ~int32`) has no such name, so
// this falls back to its full source text via types.ExprString, matching
// `gopls symbols`'s own fallback for that case.
func embeddedElementName(expr ast.Expr) string {
	if name, ok := namedTypeName(expr); ok {
		return name
	}
	return types.ExprString(expr)
}

// namedTypeName returns expr's unqualified type name and true if expr
// names a single type (an identifier, a package-qualified identifier, a
// pointer to either, or a generic instantiation of either) -- the shapes
// Go allows as an embedded field or embedded interface. ok is false for
// anything else (a union or tilde type-set element, or any other
// expression), which has no name of its own.
func namedTypeName(expr ast.Expr) (string, bool) {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch e := expr.(type) {
	case *ast.IndexExpr:
		return namedTypeName(e.X)
	case *ast.IndexListExpr:
		return namedTypeName(e.X)
	case *ast.Ident:
		return e.Name, true
	case *ast.SelectorExpr:
		return e.Sel.Name, true
	default:
		return "", false
	}
}
