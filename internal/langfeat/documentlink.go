package langfeat

import (
	"strconv"

	"github.com/sivchari/golance/internal/check"
)

// ImportLink is one import spec's source range and (unquoted) import path.
type ImportLink struct {
	Range   Range
	PkgPath string
}

// ImportLinks returns the source range and import path of every import spec
// in file. Resolving PkgPath to an actual link target (a local file, or a
// pkg.go.dev URL) needs the workspace's import graph, which this package
// does not have access to — that is the server layer's job.
func ImportLinks(cp *check.CheckedPackage, file string) ([]ImportLink, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return nil, err
	}
	out := make([]ImportLink, 0, len(astFile.Imports))
	for _, imp := range astFile.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		out = append(out, ImportLink{Range: rangeOf(tf, imp.Path.Pos(), imp.Path.End()), PkgPath: path})
	}
	return out, nil
}
