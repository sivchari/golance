package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
)

// ImportPathDefinition resolves offset (a byte offset from the start of
// file) to the import path of the import spec whose quoted path string
// contains it, for handleDefinition's "cursor is inside an import path"
// case. Facts extraction records no types.Object use for an
// *ast.ImportSpec's path -- it is a plain string literal, not an
// identifier -- so SamePackageDefinition/DependencyDefinition, both keyed
// on identAt finding an *ast.Ident at the cursor, can never answer this on
// their own; this instead walks cp's own parsed AST directly, mirroring
// ImportLinks' identical "facts index has nothing for this node" resolution
// (see its doc). It returns ("", false) if offset is not inside any import
// spec's path string or its path fails to unquote.
func ImportPathDefinition(cp *check.CheckedPackage, file string, offset int) (pkgPath string, ok bool) {
	astFile, pos, _, err := locate(cp, file, offset)
	if err != nil {
		return "", false
	}
	for _, imp := range astFile.Imports {
		// <= End (not < End) accepts a query right after the closing quote,
		// matching gopls's own importDefinition tolerance for the same case.
		if pos < imp.Path.Pos() || pos > imp.Path.End() {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return "", false
		}
		return path, true
	}
	return "", false
}

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
	obj := embeddedFieldTarget(cp.Info(), id, cp.Info().ObjectOf(id))
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

// embeddedFieldTarget redirects obj to the type it names, when id is an
// embedded field's declaring identifier and obj is the implicit field
// [types.Info.ObjectOf] resolves it to. An embedded field's identifier both
// declares the field (recorded in info.Defs) and, simultaneously, names the
// embedded type as a type expression (recorded in info.Uses) -- the same
// dual role gopls's own definition handler special-cases for the same
// reason (golang/go#42254): "Go to Definition" on the field's name should
// jump to the embedded TYPE's declaration, not back to the field's own
// (identical) position, which -- since ObjectOf prefers Defs over Uses --
// would otherwise resolve to itself and never leave the cursor's current
// position. Every other identifier kind passes through unchanged: only an
// embedded field's *types.Var can carry this ambiguity at all.
func embeddedFieldTarget(info *types.Info, id *ast.Ident, obj types.Object) types.Object {
	v, ok := obj.(*types.Var)
	if !ok || !v.Embedded() {
		return obj
	}
	if typeName := info.Uses[id]; typeName != nil {
		return typeName
	}
	return obj
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
	obj := embeddedFieldTarget(cp.Info(), id, cp.Info().ObjectOf(id))
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
