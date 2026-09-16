package langfeat

import (
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/objectpath"
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
// already-parsed files and FileSet — exact to the column, like
// DependencyDefinitionInfo's own position (see DependencyDefinition's doc),
// but resolved from cp itself rather than a depcheck.Provider.
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
// AST/types.Info/FileSet needs no index, unlike a declaration recorded
// there.
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

// BuiltinDefInfo is the result of a BuiltinDefinition query: where a
// universe (predeclared) identifier — or the error interface's Error
// method — is declared in the toolchain's $GOROOT/src/builtin/builtin.go,
// the pseudo-package gopls resolves these same identifiers against (see
// gopls@v0.23.0's internal/golang/definition.go, builtinDecl). Unlike
// SamePackageDefInfo/DependencyDefinitionInfo's positions, builtin.go is
// parsed into its own dedicated *token.FileSet (see resolveBuiltinIdent),
// never cp's own or a depcheck.Provider's, so this carries a plain 1-based
// line/column span instead — the same shape DependencyDefinitionInfo uses,
// letting the server layer's existing correctResultRange conversion
// (dirty-buffer-aware, stat-guarded) handle it identically.
type BuiltinDefInfo struct {
	Filename string
	Line     int
	Col      int
	EndCol   int
}

// BuiltinDefinition resolves the identifier at offset (a byte offset from
// the start of file) to its declaration in builtin.go, for a universe
// (predeclared) object — nil, error, len, int, iota, true, any, ... — or
// the error interface's Error method, all identified by
// types.Object.Pkg() == nil. It returns (nil, nil) if offset is not on an
// identifier, the identifier resolves to no object or a non-builtin one
// (Pkg() != nil — see SamePackageDefinition/DependencyDefinition for those
// cases instead), or GOROOT/builtin.go could not be resolved or parsed (a
// missing or corrupt toolchain install — degraded to "no result" like
// every other resolution miss in this file, not an error, since there is
// nothing else to try).
func BuiltinDefinition(cp *check.CheckedPackage, file string, offset int) (*BuiltinDefInfo, error) {
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
	if obj == nil || obj.Pkg() != nil {
		return nil, nil
	}
	info, ok := builtinDefInfoFor(obj)
	if !ok {
		return nil, nil
	}
	return info, nil
}

// builtinDefInfoFor resolves obj -- a universe (predeclared) object or the
// error interface's Error method, both identified by types.Object.Pkg()
// == nil -- into a BuiltinDefInfo, the shared conversion BuiltinDefinition
// above and TypeDefinition (internal/langfeat/typedef.go, for a predeclared
// named type like error) both need. ok is false for the same reasons
// resolveBuiltinIdent's own ok is: obj is not actually a builtin, or
// GOROOT/builtin.go could not be resolved or parsed.
func builtinDefInfoFor(obj types.Object) (*BuiltinDefInfo, bool) {
	declID, _, fset, ok := resolveBuiltinIdent(obj)
	if !ok {
		return nil, false
	}
	start := fset.Position(declID.Pos())
	end := fset.Position(declID.End())
	return &BuiltinDefInfo{Filename: start.Filename, Line: start.Line, Col: start.Column, EndCol: end.Column}, true
}

// DependencyDefinitionInfo is the result of a DependencyDefinition query:
// where the identifier at the cursor is declared, for an object outside the
// checked package. Unlike the export-data-era result this replaces, Col and
// EndCol are the real, byte-exact column span of the declaring identifier
// (a depcheck.Provider type-checks the dependency's own real source — see
// DependencyDefinition's doc), not a degraded column-1 placeholder.
type DependencyDefinitionInfo struct {
	PkgPath  string
	Filename string
	Line     int
	Col      int
	EndCol   int
}

// DependencyDefinition resolves the identifier at offset (a byte offset from
// the start of file) to the types.Object it refers to, and returns exactly
// where that object is declared: dp type-checks the dependency's own real
// source files (internal/depcheck.Provider, never compiler/gcexportdata
// export data) and locates the declaring identifier there directly, so the
// result is exact to the column and — unlike the previous export-data path
// — resolves an unexported dependency object too (reachable, e.g., when cp
// is itself a dependency package navigated into from a stdlib/module-cache
// file already open; see Provider.Decl's doc for exactly which unexported
// cases this covers). It returns (nil, nil) if offset is not on an
// identifier, the identifier resolves to no object, the object is
// predeclared (e.g. error, any — no Pkg()), or the object is declared in
// cp's own package: the caller already has a source position for that case
// (see internal/xref's workspace facts index, or SamePackageDefinition) and
// should prefer it.
func DependencyDefinition(ctx context.Context, cp *check.CheckedPackage, dp *depcheck.Provider, file string, offset int) (*DependencyDefinitionInfo, error) {
	obj, err := dependencyDefinitionObject(cp, file, offset)
	if err != nil || obj == nil {
		return nil, err
	}
	declID, fset, err := dp.Decl(ctx, obj.Pkg().Path(), obj)
	if err != nil {
		return nil, err
	}
	start := fset.Position(declID.Pos())
	end := fset.Position(declID.End())
	return &DependencyDefinitionInfo{
		PkgPath:  obj.Pkg().Path(),
		Filename: start.Filename,
		Line:     start.Line,
		Col:      start.Column,
		EndCol:   end.Column,
	}, nil
}

// dependencyDefinitionObject resolves the identifier at offset to the
// types.Object DependencyDefinition and DependencyDefinitionTarget both
// need, sharing its "not a dependency identifier" declines: nil (with a nil
// error) if offset is not on an identifier, the identifier resolves to no
// object, the object is predeclared (no Pkg()), or it is declared in cp's
// own package.
func dependencyDefinitionObject(cp *check.CheckedPackage, file string, offset int) (types.Object, error) {
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
	return obj, nil
}

// DependencyDefinitionTarget resolves the identifier at offset to the
// (package path, objectpath) pair internal/index's facts extraction would
// have recorded for it -- the same [objectpath.For] plus
// [depcheck.OriginObject] computation facts.go's symbolID uses -- so a
// caller can check the on-disk facts index (an O(1) DB read, via
// [internal/xref.Resolver.TypeDeclaration]) before falling back to
// DependencyDefinition's much slower source-checking Decl path. ok is false
// under the same conditions DependencyDefinition declines on, plus obj not
// being reachable via objectpath (e.g. a function-local declaration).
func DependencyDefinitionTarget(cp *check.CheckedPackage, file string, offset int) (pkgPath, objPath string, ok bool) {
	obj, err := dependencyDefinitionObject(cp, file, offset)
	if err != nil || obj == nil {
		return "", "", false
	}
	op, err := objectpath.For(depcheck.OriginObject(obj))
	if err != nil {
		return "", "", false
	}
	return obj.Pkg().Path(), string(op), true
}

// PackageNameDefinition resolves the identifier at offset (a byte offset
// from the start of file) to the import spec that declares it, for the
// case where offset lands on a package-QUALIFIER identifier -- the "os" in
// "os.Getenv(...)" -- rather than the selector after the dot.
// SamePackageDefinition and DependencyDefinition both decline this case:
// a *types.PkgName's Pkg() reports the IMPORTING package (cp's own, since
// the qualifier is a file-scoped name, not a member of the imported
// package), so both take it for a same-package identifier and try to find
// an *ast.Ident at the PkgName's own Pos() -- which, for a plain,
// unaliased import ("\"os\""), is the position of the quoted path string
// literal, not an identifier at all, so identAt finds nothing and both
// silently decline. It returns (nil, nil) if offset is not on an
// identifier or the identifier does not resolve to a *types.PkgName.
//
// The returned span matches gopls's own choice of target for the same
// query (verified against gopls@v0.23.0): for an explicitly aliased
// import ("f \"pkg\""), the alias identifier; for a plain import, the
// whole quoted path string literal -- exactly go/types' own
// PkgName.Pos()/End() for each case, so this needs no position arithmetic
// beyond locating the import spec that declared the resolved PkgName (see
// packageNameDeclRange).
func PackageNameDefinition(cp *check.CheckedPackage, file string, offset int) (*SamePackageDefInfo, error) {
	astFile, pos, tf, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, nil
	}
	pn, ok := cp.Info().ObjectOf(id).(*types.PkgName)
	if !ok {
		return nil, nil
	}
	rg, ok := packageNameDeclRange(cp, astFile, tf, pn)
	if !ok {
		return nil, nil
	}
	return &SamePackageDefInfo{File: tf.Name(), Range: rg}, nil
}

// PackageNameReferences resolves the identifier at offset (a byte offset
// from the start of file) to every use of the same *types.PkgName within
// file -- every "os." qualifier in a file importing "os", for example --
// for handleReferences' file-local fallback when the workspace facts index
// has nothing for a package-qualifier identifier (facts extraction never
// records a *types.PkgName as an indexable symbol; see
// PackageNameDefinition's doc for the identical gap on the definition
// side). includeDecl additionally includes the import spec's own span
// (PackageNameDefinition's target). It returns (nil, nil) if offset is not
// on an identifier or the identifier does not resolve to a *types.PkgName.
//
// This only searches file itself, not the whole workspace: a package
// qualifier is local to the importing file by construction (Go has no
// cross-file "using" declaration for an import), so every reference to it
// is already in file -- closing the scope a user expects clicking a
// qualifier without needing the facts index, which does not track this
// symbol kind at all, to answer.
func PackageNameReferences(cp *check.CheckedPackage, file string, offset int, includeDecl bool) ([]Range, error) {
	astFile, pos, tf, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, nil
	}
	pn, ok := cp.Info().ObjectOf(id).(*types.PkgName)
	if !ok {
		return nil, nil
	}
	var ranges []Range
	if includeDecl {
		if declRange, ok := packageNameDeclRange(cp, astFile, tf, pn); ok {
			ranges = append(ranges, declRange)
		}
	}
	ast.Inspect(astFile, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if cp.Info().Uses[ident] == pn {
			ranges = append(ranges, rangeOf(tf, ident.Pos(), ident.End()))
		}
		return true
	})
	return ranges, nil
}

// packageNameDeclRange returns the span PackageNameDefinition/
// PackageNameReferences point a *types.PkgName's declaration at: among
// astFile's own imports, the one that declared pn -- matched by identity
// against cp.Info()'s Defs (an explicit alias) or Implicits (a plain,
// unaliased import; see go/types' own documented use of Implicits for an
// ImportSpec's implicit PkgName) -- rather than by re-deriving obj.Pos()'s
// enclosing node, since either map lookup already pins the exact
// *ast.ImportSpec responsible without a second AST walk.
func packageNameDeclRange(cp *check.CheckedPackage, astFile *ast.File, tf *token.File, pn *types.PkgName) (Range, bool) {
	for _, imp := range astFile.Imports {
		if imp.Name != nil {
			if cp.Info().Defs[imp.Name] == pn {
				return rangeOf(tf, imp.Name.Pos(), imp.Name.End()), true
			}
			continue
		}
		if cp.Info().Implicits[imp] == pn {
			return rangeOf(tf, imp.Path.Pos(), imp.Path.End()), true
		}
	}
	return Range{}, false
}
