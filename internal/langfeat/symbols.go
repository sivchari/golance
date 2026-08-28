package langfeat

import (
	"go/ast"
	"go/token"

	"github.com/sivchari/golance/internal/check"
)

// SymbolKind categorizes a Symbol, independent of any LSP protocol type.
type SymbolKind int

// Kinds a Symbol can have.
const (
	SymbolFunc SymbolKind = iota
	SymbolMethod
	SymbolType
	SymbolVar
	SymbolConst
)

// Symbol is one declaration in a file's outline. Methods are nested under
// the Symbol for their receiver type when that type is declared in the
// same file; otherwise they appear at the top level alongside it.
type Symbol struct {
	Name     string
	Kind     SymbolKind
	Range    Range
	Children []Symbol
}

// DocumentSymbols returns file's declarations as a hierarchical outline:
// top-level types, funcs, vars, and consts, with methods nested under
// their receiver type.
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
				Name:  ts.Name.Name,
				Kind:  SymbolType,
				Range: rangeOf(tf, ts.Pos(), ts.End()),
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
