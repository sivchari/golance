package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sivchari/golance/internal/check"
)

// goGenerateDirective is the exact "//go:generate" comment prefix gopls's
// own goGenerateCodeLens recognizes (golang.org/x/tools/gopls/internal/
// golang/code_lens.go).
const goGenerateDirective = "//go:generate"

// GenerateLens is FindGenerateLens's result: the directory `go generate`
// should run in, and the Range of the file's first "//go:generate"
// directive comment.
type GenerateLens struct {
	Dir   string
	Range Range
}

// FindGenerateLens finds file's go:generate code lens location, mirroring
// gopls's own goGenerateCodeLens exactly: only the FIRST
// "//go:generate"-prefixed comment in the file produces a lens location —
// gopls returns as soon as it finds one, so every later directive in the
// same file is deliberately ignored, unlike `go generate` itself, which
// runs every directive it finds. The caller (see
// internal/server/handlers_codelens.go) emits two commands at the returned
// Range, a recursive and a non-recursive "run go generate", matching
// gopls's own pair.
func FindGenerateLens(cp *check.CheckedPackage, file string) (GenerateLens, bool, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return GenerateLens{}, false, err
	}
	for _, cg := range astFile.Comments {
		for _, c := range cg.List {
			if !strings.HasPrefix(c.Text, goGenerateDirective) {
				continue
			}
			end := c.Pos() + token.Pos(len(goGenerateDirective))
			return GenerateLens{
				Dir:   filepath.Dir(file),
				Range: rangeOf(tf, c.Pos(), end),
			}, true, nil
		}
	}
	return GenerateLens{}, false, nil
}

// RegenerateCgoLens is FindRegenerateCgoLens's result: the Range of file's
// `import "C"` declaration.
type RegenerateCgoLens struct {
	Range Range
}

// FindRegenerateCgoLens finds file's `import "C"` declaration, mirroring
// gopls's own regenerateCgoLens: its unbroken loop over every import in the
// file means that if more than one `import "C"` line somehow exists (never
// legal Go, but AST-level detection does not care), the LAST one found
// wins — matched here for parity even though it cannot occur in valid Go
// source.
func FindRegenerateCgoLens(cp *check.CheckedPackage, file string) (RegenerateCgoLens, bool, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return RegenerateCgoLens{}, false, err
	}
	var found *ast.ImportSpec
	for _, imp := range astFile.Imports {
		if imp.Path.Value == `"C"` {
			found = imp
		}
	}
	if found == nil {
		return RegenerateCgoLens{}, false, nil
	}
	return RegenerateCgoLens{Range: rangeOf(tf, found.Pos(), found.End())}, true, nil
}

var (
	testCodeLensRe      = regexp.MustCompile(`^Test([^a-z]|$)`)      // TestFoo or Test but not Testable
	benchmarkCodeLensRe = regexp.MustCompile(`^Benchmark([^a-z]|$)`) // BenchmarkFoo or Benchmark but not Benchmarkable
)

// TestFuncLens is one Test or Benchmark function TestAndBenchmarkLenses
// finds: its name, and the zero-width Range at its own *ast.FuncDecl's
// start position (gopls anchors the lens there, not over the whole
// declaration).
type TestFuncLens struct {
	Name  string
	Range Range
}

// TestAndBenchmarkLenses returns file's top-level Test and Benchmark
// functions, recognized exactly as gopls's own testsAndBenchmarks /
// matchTestFunc do (golang.org/x/tools/gopls/internal/golang/code_lens.go):
// a `^Test([^a-z]|$)` / `^Benchmark([^a-z]|$)` name match AND a signature
// check — exactly one parameter, of type *testing.T or *testing.B — so a
// same-named non-test helper (e.g. `func TestHelper(t *testing.T, want
// int)`, extra parameter; or `func Test(x int)`, wrong parameter type) is
// excluded even though its name alone would match the regexp. Only
// _test.go files have lenses at all — anything else returns (nil, nil,
// nil), not an error. A Fuzz or Example function is never lensed (gopls's
// own source has no fuzz/example regexp at all), and neither is a subtest
// registered with t.Run inside a Test function's body, since it is never a
// top-level *ast.FuncDecl the declaration scan below visits.
func TestAndBenchmarkLenses(cp *check.CheckedPackage, file string) (tests, benchmarks []TestFuncLens, err error) {
	if !strings.HasSuffix(file, "_test.go") {
		return nil, nil, nil
	}
	astFile, tf, ferr := astFileByName(cp, file)
	if ferr != nil {
		return nil, nil, ferr
	}
	info := cp.Info()
	for _, decl := range astFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		rng := rangeOf(tf, fn.Pos(), fn.Pos())
		switch {
		case matchTestFunc(fn, info, testCodeLensRe, "T"):
			tests = append(tests, TestFuncLens{Name: fn.Name.Name, Range: rng})
		case matchTestFunc(fn, info, benchmarkCodeLensRe, "B"):
			benchmarks = append(benchmarks, TestFuncLens{Name: fn.Name.Name, Range: rng})
		}
	}
	return tests, benchmarks, nil
}

// matchTestFunc reports whether fn is a Test/Benchmark function gopls's own
// runTestCodeLens would lens: its name matches nameRe AND its sole
// parameter is a pointer to the "testing" package's T or B type (named by
// paramName) — see TestAndBenchmarkLenses' doc for why both checks matter.
func matchTestFunc(fn *ast.FuncDecl, info *types.Info, nameRe *regexp.Regexp, paramName string) bool {
	if !nameRe.MatchString(fn.Name.Name) {
		return false
	}
	obj, ok := info.ObjectOf(fn.Name).(*types.Func)
	if !ok {
		return false
	}
	sig := obj.Signature()
	if sig.Params().Len() != 1 {
		return false
	}
	ptr, ok := sig.Params().At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return false
	}
	paramObj := named.Obj()
	return paramObj.Pkg() != nil && paramObj.Pkg().Path() == "testing" && paramObj.Name() == paramName
}

// FileBenchmarksRange returns the zero-width Range at file's "package"
// keyword — where gopls's own runTestCodeLens places its "run file
// benchmarks" lens when file has at least one Benchmark function (see
// TestAndBenchmarkLenses).
func FileBenchmarksRange(cp *check.CheckedPackage, file string) (Range, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return Range{}, err
	}
	return rangeOf(tf, astFile.Package, astFile.Package), nil
}
