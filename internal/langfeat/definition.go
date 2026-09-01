package langfeat

import (
	"go/token"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
)

// SamePackageDefInfo is the result of a SamePackageDefinition query: the
// identifier's own declaring identifier, located entirely from cp's
// already-parsed files and FileSet — unlike DependencyDefinitionInfo's
// export-data position, this is exact to the column, not just the line.
type SamePackageDefInfo struct {
	File  string
	Range Range
}

// SamePackageDefinition resolves the identifier at offset (a byte offset
// from the start of file) to its own declaring identifier, for the case
// DependencyDefinition declines: an object declared in cp's own package.
// It returns (nil, nil) if offset is not on an identifier, the identifier
// resolves to no object, the object is predeclared (e.g. error, any — no
// Pkg()), or the object is declared in a different package (see
// DependencyDefinition for that case instead).
//
// This exists for handleDefinition's fallback when the workspace facts
// index cannot answer at all: resolving straight from cp's own
// AST/types.Info/FileSet needs no index and, unlike a declaration recorded
// there or DependencyDefinition's export-data position, is exact down to
// the column.
func SamePackageDefinition(cp *check.CheckedPackage, file string, offset int) (*SamePackageDefInfo, error) {
	astFile, pos, _, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, nil
	}
	obj := cp.Info().ObjectOf(id)
	if obj == nil || obj.Pkg() == nil || obj.Pkg() != cp.Package() {
		return nil, nil
	}
	if !obj.Pos().IsValid() {
		return nil, nil
	}
	declFile, tf, ok := fileContaining(cp, obj.Pos())
	if !ok {
		return nil, nil
	}
	declPath, _ := astutil.PathEnclosingInterval(declFile, obj.Pos(), obj.Pos())
	declID := identAt(declPath)
	if declID == nil {
		return nil, nil
	}
	return &SamePackageDefInfo{File: tf.Name(), Range: rangeOf(tf, declID.Pos(), declID.End())}, nil
}

// DependencyDefinitionInfo is the result of a DependencyDefinition query:
// where the identifier at the cursor is declared, for an object outside the
// checked package.
type DependencyDefinitionInfo struct {
	PkgPath  string
	Filename string
	Line     int
}

// goRootPlaceholder is the literal string gcexportdata leaves in a stdlib
// package's export-data file paths in place of the actual GOROOT, for build
// reproducibility (see cmd/internal/objabi.AbsFile upstream). Callers must
// expand it themselves; see expandGoroot.
const goRootPlaceholder = "$GOROOT"

// DependencyDefinition resolves the identifier at offset (a byte offset from
// the start of file) to the types.Object it refers to, and returns where
// that object is declared, resolved through depFset — the *token.FileSet
// the dependency importer decoded cp's dependencies' export data into (see
// internal/typecheck.Importer, internal/server's depCacheHolder). It returns
// (nil, nil) if offset is not on an identifier, the identifier resolves to
// no object, the object is predeclared (e.g. error, any — no Pkg()), or the
// object is declared in cp's own package: the caller already has a source
// position for that case (see internal/xref's workspace facts index) and
// should prefer it.
//
// Unlike a declaration recorded in the workspace facts index, export data
// does not preserve column information (see internal/xref.methodFuncLocation's
// doc), so the returned position always addresses the start of the
// declaration's line.
func DependencyDefinition(cp *check.CheckedPackage, depFset *token.FileSet, file string, offset int) (*DependencyDefinitionInfo, error) {
	astFile, pos, _, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, nil
	}
	obj := cp.Info().ObjectOf(id)
	if obj == nil || obj.Pkg() == nil || obj.Pkg() == cp.Package() {
		return nil, nil
	}
	objPos := obj.Pos()
	if !objPos.IsValid() {
		return nil, nil
	}
	tpos := depFset.Position(objPos)
	if !tpos.IsValid() || tpos.Line <= 0 {
		return nil, nil
	}
	return &DependencyDefinitionInfo{
		PkgPath:  obj.Pkg().Path(),
		Filename: expandGoroot(tpos.Filename),
		Line:     tpos.Line,
	}, nil
}

// expandGoroot replaces a leading $GOROOT placeholder (see
// goRootPlaceholder) with the toolchain's actual GOROOT — the same
// substitution golang.org/x/tools' own internal gcimporter tests apply to
// positions decoded from stdlib export data. A module dependency's export
// data already carries an absolute path and passes through unchanged. If
// GOROOT cannot be determined the placeholder is left as is; the caller's
// file-exists check then rejects the location, degrading to no result.
func expandGoroot(filename string) string {
	if !strings.HasPrefix(filename, goRootPlaceholder) {
		return filename
	}
	root := goroot()
	if root == "" {
		return filename
	}
	return strings.Replace(filename, goRootPlaceholder, root, 1)
}

// goroot resolves the toolchain's GOROOT once: "go env GOROOT" is the
// supported way to locate it (runtime.GOROOT is deprecated since Go 1.24
// and wrong for a relocated binary), with the GOROOT environment variable
// as a fallback when the go binary is not on PATH.
var goroot = sync.OnceValue(func() string {
	if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root
		}
	}
	return os.Getenv("GOROOT")
})
