package langfeat

import (
	"fmt"
	"go/ast"
	"go/token"

	"github.com/sivchari/golance/internal/check"
)

// Range is a source range expressed as byte offsets from the start of a
// file, the same coordinate system every position in this package's public
// API uses.
type Range struct {
	StartOffset int
	EndOffset   int
}

// astFileByName returns cp's parsed *ast.File for file, along with the
// token.File fset resolves its positions against.
func astFileByName(cp *check.CheckedPackage, file string) (*ast.File, *token.File, error) {
	for _, f := range cp.Files() {
		tf := cp.FileSet().File(f.Pos())
		if tf != nil && tf.Name() == file {
			return f, tf, nil
		}
	}
	return nil, nil, fmt.Errorf("langfeat: %s is not part of the checked package", file)
}

// posForOffset converts a byte offset into tf's file to a token.Pos, or an
// error if offset falls outside the file.
func posForOffset(tf *token.File, offset int) (token.Pos, error) {
	if offset < 0 || offset > tf.Size() {
		return token.NoPos, fmt.Errorf("langfeat: offset %d out of range for %s (size %d)", offset, tf.Name(), tf.Size())
	}
	return tf.Pos(offset), nil
}

// locate resolves file and offset to the parsed *ast.File containing it,
// the corresponding token.Pos, and the token.File used to convert back to
// byte offsets.
func locate(cp *check.CheckedPackage, file string, offset int) (*ast.File, token.Pos, *token.File, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return nil, token.NoPos, nil, err
	}
	pos, err := posForOffset(tf, offset)
	if err != nil {
		return nil, token.NoPos, nil, err
	}
	return astFile, pos, tf, nil
}

// rangeOf converts a [start, end) token.Pos span in tf's file to a Range.
func rangeOf(tf *token.File, start, end token.Pos) Range {
	return Range{StartOffset: tf.Offset(start), EndOffset: tf.Offset(end)}
}
