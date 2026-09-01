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

	// texts holds the exact source bytes each of files was parsed from,
	// keyed by path — the same content contentHash hashed and go/types
	// checked against. See FileText.
	texts map[string][]byte

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

// FromSource builds a CheckedPackage from an already source-type-checked,
// non-workspace package (internal/depcheck.Provider's real-source pipeline)
// rather than running Engine's own recheck: the unification point for a
// file opened INSIDE a dependency whose directory the import graph already
// knows (see internal/depcheck's package doc and Engine's own doc for why
// Engine's ad-hoc pipeline stays reserved for directories the graph does
// not). files/pkg/info/fset are exactly what depcheck already produced (no
// separate parse or type-check happens here); texts is read directly by the
// caller (module-cache/GOROOT files are immutable, but a caller may still
// have one open with unsaved overlay edits) since depcheck's own
// CheckedPackage carries no text cache of its own. There is no parse/type
// error list to populate (depcheck's own check runs best-effort, discarding
// individual type errors — see its doc) and no contentHash: this
// CheckedPackage is never entered into Engine's own cache or its
// content-hash-keyed staleness check, so both are left at their zero value.
func FromSource(pkgPath, dir string, fset *token.FileSet, files []*ast.File, pkg *types.Package, info *types.Info, texts map[string][]byte) *CheckedPackage {
	return &CheckedPackage{
		pkgPath: pkgPath,
		dir:     dir,
		fset:    fset,
		files:   files,
		pkg:     pkg,
		info:    info,
		texts:   texts,
		builtAt: time.Now(),
	}
}

// FileText returns the exact source bytes cp was checked against for path,
// and whether path was one of the files checked. Unlike re-reading the
// overlay after the fact, this is guaranteed to match what Files and
// FileSet's positions were resolved against: a caller deriving byte
// offsets from an LSP position (or otherwise needing file content
// consistent with cp) should use this instead of its own separate overlay
// read, which could race a concurrent edit and disagree with cp.
func (cp *CheckedPackage) FileText(path string) ([]byte, bool) {
	text, ok := cp.texts[path]
	return text, ok
}

// Result is a publishable summary of a CheckedPackage, delivered to
// Options.OnResult after a successful recheck.
type Result struct {
	PkgPath string
	Dir     string
	// Files lists every file cp was actually checked against — not just the
	// ones in Diags. A directory can now hold two independent units (its
	// base package and, separately, its external "_test" package — see
	// internal/check's unitKey), each publishing its own Result for the
	// same Dir with disjoint file sets, so a caller that clears a
	// no-longer-diagnosed file's diagnostics on this Result's behalf needs
	// to know which files it is actually authoritative for, rather than
	// assuming (as a single-unit-per-directory caller safely could before)
	// that it speaks for every open file in Dir.
	Files   []string
	Diags   []Diag
	BuiltAt time.Time
}

// newResult builds a Result from cp and its diagnostics.
func newResult(cp *CheckedPackage, diags []Diag) *Result {
	files := make([]string, 0, len(cp.files))
	for _, f := range cp.files {
		if tf := cp.fset.File(f.Pos()); tf != nil {
			files = append(files, tf.Name())
		}
	}
	return &Result{
		PkgPath: cp.pkgPath,
		Dir:     cp.dir,
		Files:   files,
		Diags:   diags,
		BuiltAt: cp.builtAt,
	}
}
