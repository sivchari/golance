package langfeat

import (
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
)

// PrepareRename reports whether the identifier at offset (a byte offset
// from the start of file) can be renamed, and if so, its current source
// range. It returns (nil, nil) if offset is not on an identifier (e.g. a
// keyword or punctuation), or the identifier names an import ("package
// name"), neither of which this server supports renaming.
func PrepareRename(cp *check.CheckedPackage, file string, offset int) (*Range, error) {
	astFile, pos, tf, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, nil
	}
	obj := cp.Info().ObjectOf(id)
	if obj == nil {
		return nil, nil // e.g. the package clause's own name, which denotes no object
	}
	if _, isPkgName := obj.(*types.PkgName); isPkgName {
		return nil, nil
	}
	r := rangeOf(tf, id.Pos(), id.End())
	return &r, nil
}
