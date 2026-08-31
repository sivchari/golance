package langfeat

import (
	"go/ast"
	"go/token"
	"sort"

	"github.com/sivchari/golance/internal/check"
)

// FoldingKind categorizes a FoldingRangeInfo, independent of any LSP
// protocol type.
type FoldingKind int

// Kinds a FoldingRangeInfo can have.
const (
	FoldRegion FoldingKind = iota
	FoldComment
	FoldImports
)

// FoldingRangeInfo is one foldable region of a file.
type FoldingRangeInfo struct {
	Range Range
	Kind  FoldingKind
}

// FoldingRanges returns every foldable region in file: block statements
// (func bodies, if/for/switch/select bodies, nested arbitrarily deeply),
// struct/interface type bodies, import blocks, and multi-line comment
// groups.
func FoldingRanges(cp *check.CheckedPackage, file string) ([]FoldingRangeInfo, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return nil, err
	}

	var out []FoldingRangeInfo
	out = append(out, importFoldingRanges(tf, astFile)...)
	out = append(out, commentFoldingRanges(tf, astFile)...)
	ast.Inspect(astFile, func(n ast.Node) bool {
		if fr, ok := blockFoldingRange(tf, n); ok {
			out = append(out, fr)
		}
		return true
	})

	sort.Slice(out, func(i, j int) bool { return out[i].Range.StartOffset < out[j].Range.StartOffset })
	return out, nil
}

// blockFoldingRange returns n's foldable range if n is a block statement or
// a struct/interface type body spanning more than one line.
func blockFoldingRange(tf *token.File, n ast.Node) (FoldingRangeInfo, bool) {
	switch b := n.(type) {
	case *ast.BlockStmt:
		return spanningFoldingRange(tf, b.Lbrace, b.Rbrace, FoldRegion)
	case *ast.StructType:
		if b.Fields == nil {
			return FoldingRangeInfo{}, false
		}
		return spanningFoldingRange(tf, b.Fields.Opening, b.Fields.Closing, FoldRegion)
	case *ast.InterfaceType:
		if b.Methods == nil {
			return FoldingRangeInfo{}, false
		}
		return spanningFoldingRange(tf, b.Methods.Opening, b.Methods.Closing, FoldRegion)
	}
	return FoldingRangeInfo{}, false
}

// spanningFoldingRange returns a FoldingRangeInfo for [open, closePos] if
// they fall on different lines (nothing to fold otherwise).
func spanningFoldingRange(tf *token.File, open, closePos token.Pos, kind FoldingKind) (FoldingRangeInfo, bool) {
	if !open.IsValid() || !closePos.IsValid() || tf.Line(open) == tf.Line(closePos) {
		return FoldingRangeInfo{}, false
	}
	return FoldingRangeInfo{Range: rangeOf(tf, open, closePos), Kind: kind}, true
}

// importFoldingRanges returns one FoldingRangeInfo per parenthesized import
// block in f.
func importFoldingRanges(tf *token.File, f *ast.File) []FoldingRangeInfo {
	var out []FoldingRangeInfo
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		if fr, ok := spanningFoldingRange(tf, gd.Lparen, gd.Rparen, FoldImports); ok {
			out = append(out, fr)
		}
	}
	return out
}

// commentFoldingRanges returns one FoldingRangeInfo per multi-line comment
// group in f.
func commentFoldingRanges(tf *token.File, f *ast.File) []FoldingRangeInfo {
	var out []FoldingRangeInfo
	for _, cg := range f.Comments {
		if fr, ok := spanningFoldingRange(tf, cg.Pos(), cg.End(), FoldComment); ok {
			out = append(out, fr)
		}
	}
	return out
}
