package check

import (
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"time"
)

// CheckedPackage is the result of type-checking one package: its parsed
// files, resolved *types.Package and *types.Info, and every parse/type
// error encountered along the way. Callers outside this package obtain one
// only through Engine.Get and read it through the accessor methods below.
type CheckedPackage struct {
	pkgPath string
	dir     string

	fset  *token.FileSet
	files []*ast.File
	pkg   *types.Package
	info  *types.Info

	parseErrs scanner.ErrorList
	typeErrs  []types.Error

	contentHash string
	builtAt     time.Time
}

// PkgPath returns the package's import path.
func (cp *CheckedPackage) PkgPath() string { return cp.pkgPath }

// Dir returns the package's directory.
func (cp *CheckedPackage) Dir() string { return cp.dir }

// FileSet returns the token.FileSet Files and Package positions are
// resolved against.
func (cp *CheckedPackage) FileSet() *token.FileSet { return cp.fset }

// Files returns the package's parsed files, in the order they were checked.
// A file with syntax errors is still present, as a partial AST.
func (cp *CheckedPackage) Files() []*ast.File { return cp.files }

// Package returns the checked *types.Package. It may be incomplete if
// TypeErrors is non-empty.
func (cp *CheckedPackage) Package() *types.Package { return cp.pkg }

// Info returns the *types.Info populated by the check (Defs, Uses,
// Selections, Types, Scopes, Instances, Implicits).
func (cp *CheckedPackage) Info() *types.Info { return cp.info }

// Result is a publishable summary of a CheckedPackage, delivered to
// Options.OnResult after a successful recheck.
type Result struct {
	PkgPath string
	Dir     string
	Diags   []Diag
	BuiltAt time.Time
}

// newResult builds a Result from cp and its diagnostics.
func newResult(cp *CheckedPackage, diags []Diag) *Result {
	return &Result{
		PkgPath: cp.pkgPath,
		Dir:     cp.dir,
		Diags:   diags,
		BuiltAt: cp.builtAt,
	}
}
