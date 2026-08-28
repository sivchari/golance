package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
)

// SigInfo is the result of a SignatureHelp query: the called function's
// signature, its parameters rendered individually, and which one the
// cursor is currently in.
type SigInfo struct {
	Label       string
	Params      []string
	ActiveParam int
}

// SignatureHelp resolves the call expression enclosing offset (a byte
// offset from the start of file) to its signature and active parameter
// index. It returns (nil, nil) if offset is not inside a call to a
// function or method with a resolvable signature.
func SignatureHelp(cp *check.CheckedPackage, file string, offset int) (*SigInfo, error) {
	astFile, pos, _, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	call := enclosingCall(path)
	if call == nil {
		return nil, nil
	}
	sig, ok := cp.Info().TypeOf(call.Fun).(*types.Signature)
	if !ok {
		return nil, nil
	}

	params := make([]string, sig.Params().Len())
	for i := range sig.Params().Len() {
		params[i] = types.ObjectString(sig.Params().At(i), types.RelativeTo(cp.Package()))
	}

	return &SigInfo{
		Label:       types.TypeString(sig, types.RelativeTo(cp.Package())),
		Params:      params,
		ActiveParam: activeParamIndex(call, pos, sig),
	}, nil
}

// enclosingCall returns the nearest *ast.CallExpr in path, or nil if path
// contains none.
func enclosingCall(path []ast.Node) *ast.CallExpr {
	for _, n := range path {
		if call, ok := n.(*ast.CallExpr); ok {
			return call
		}
	}
	return nil
}

// activeParamIndex returns the index into sig.Params() that pos falls in,
// among call's arguments: the first argument whose end is at or after pos,
// or one past the last argument if pos is beyond all of them. A variadic
// signature's index is capped at its last (variadic) parameter.
func activeParamIndex(call *ast.CallExpr, pos token.Pos, sig *types.Signature) int {
	idx := len(call.Args)
	for i, arg := range call.Args {
		if pos <= arg.End() {
			idx = i
			break
		}
	}
	if n := sig.Params().Len(); sig.Variadic() && idx >= n {
		idx = n - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}
