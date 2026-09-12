package check

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// mapReader is a minimal overlay.FileReader backed by an in-memory map, for
// tests that only need Diagnostics to read exactly the fixture content it
// was built from.
type mapReader map[string][]byte

func (m mapReader) ReadFile(path string) ([]byte, error) {
	text, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("mapReader: %s not found", path)
	}
	return text, nil
}

// diagnosticsGoplsParityFixture has one type error per line covering each
// of the categories identEnd/typeErrorRange must extend correctly: an
// identifier (undefined name), a string literal, an integer literal, a
// composite literal, and a call expression.
const diagnosticsGoplsParityFixture = `package p

func F() int {
	x := undefinedIdent
	var y int = "a string literal"
	var z string = 42
	var w int = []int{1, 2, 3}
	var v int = len("a", "b")
	_ = x
	_ = y
	_ = z
	_ = w
	_ = v
	return 0
}
`

// TestDiagnostics_GoplsParity compares Diagnostics's ranges against gopls
// check's own ranges (the oracle diagnostics.go's doc names) for
// diagnosticsGoplsParityFixture, one category per line. Skips if gopls is
// not on PATH.
func TestDiagnostics_GoplsParity(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	// EvalSymlinks: on macOS, t.TempDir() lands under /var/folders, itself a
	// symlink to /private/var/folders. gopls resolves the module root from
	// cmd.Dir and compares it against the (symlink-resolved) path it derives
	// while loading packages; passing the unresolved path here makes it
	// silently find no diagnostics for mainGo at all, rather than erroring.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo, []byte(diagnosticsGoplsParityFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	goMod := "module goplsparityfixture\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	want := goplsCheckRanges(t, dir, mainGo)
	if len(want) != 5 {
		t.Fatalf("goplsCheckRanges returned %d diagnostics, want 5 (one per fixture line): %+v", len(want), want)
	}

	got := diagnosticsByLine(t, mainGo, diagnosticsGoplsParityFixture)

	for line, w := range want {
		g, ok := got[line]
		if !ok {
			t.Errorf("line %d (%s): Diagnostics reported nothing, want range [%d,%d)", line+1, w.msg, w.startCol, w.endCol)
			continue
		}
		if g.startCol != w.startCol || g.endCol != w.endCol {
			t.Errorf("line %d (%s): Diagnostics range = [%d,%d), want [%d,%d) (gopls)", line+1, w.msg, g.startCol, g.endCol, w.startCol, w.endCol)
		}
	}
	for line, g := range got {
		if _, ok := want[line]; !ok {
			t.Errorf("line %d: Diagnostics reported an unexpected range [%d,%d), gopls reported none", line+1, g.startCol, g.endCol)
		}
	}
}

// diagRange is a single-line [startCol, endCol) span, both 0-based UTF-16
// columns — the coordinate system Diag itself uses.
type diagRange struct {
	startCol, endCol uint32
	msg              string
}

// goplsCheckRanges runs `gopls check mainGo` (dir set to the fixture's own
// module root, since gopls check resolves its module from the process's
// working directory rather than the file's) and parses its
// "file:line:startCol-endCol: message" output into a map keyed by 0-based
// line number. gopls's columns are 1-based with the end column already one
// past the last included character (mirrored here as -1 on both ends to
// land on Diag's own 0-based, half-open convention).
func goplsCheckRanges(t *testing.T, dir, mainGo string) map[int]diagRange {
	t.Helper()
	gocache := filepath.Join(t.TempDir(), "gocache")
	if err := os.MkdirAll(gocache, 0o750); err != nil {
		t.Fatalf("mkdir GOCACHE: %v", err)
	}
	args := []string{"check"}
	args = append(args, mainGo)
	cmd := exec.Command("gopls", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+gocache)
	out, _ := cmd.CombinedOutput() // gopls check's exit code does not reflect whether diagnostics were found

	lineRe := regexp.MustCompile(`^[^:]+:(\d+):(\d+)-(\d+): (.*)$`)
	ranges := make(map[int]diagRange)
	for _, line := range strings.Split(string(out), "\n") {
		m := lineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNo, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			t.Fatalf("parse line number %q: %v", m[1], err)
		}
		startCol, ok := parseCol1Based(t, m[2])
		if !ok {
			continue
		}
		endCol, ok := parseCol1Based(t, m[3])
		if !ok {
			continue
		}
		ranges[int(lineNo)-1] = diagRange{
			startCol: startCol,
			endCol:   endCol,
			msg:      m[4],
		}
	}
	if len(ranges) == 0 {
		t.Fatalf("no diagnostics parsed from gopls check output (dir=%s):\n%s", dir, out)
	}
	return ranges
}

// parseCol1Based parses s as a 1-based column and returns it as a 0-based
// uint32. ok is false if s is not a valid column (unparsable, or "0", which
// cannot be converted to a 0-based column without underflowing).
func parseCol1Based(t *testing.T, s string) (col uint32, ok bool) {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || v == 0 {
		t.Errorf("parse 1-based column %q: %v", s, err)
		return 0, false
	}
	return uint32(v) - 1, true
}

// diagnosticsByLine builds a CheckedPackage from src (typechecked against no
// imports, since the fixture uses only builtins) and returns Diagnostics's
// output keyed by 0-based StartLine, for a fixture with exactly one
// diagnostic per offending line.
func diagnosticsByLine(t *testing.T, path, src string) map[int]diagRange {
	t.Helper()
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		t.Fatalf("parser.ParseFile: %v", err)
	}
	pkg, info, typeErrs := typecheck.CheckPackage(fset, []*ast.File{astFile}, "p", nil)
	cp := &CheckedPackage{
		pkgPath:  "p",
		dir:      filepath.Dir(path),
		fset:     fset,
		files:    []*ast.File{astFile},
		pkg:      pkg,
		info:     info,
		typeErrs: typeErrs,
		texts:    map[string][]byte{path: []byte(src)},
	}
	reader := mapReader{path: []byte(src)}

	ranges := make(map[int]diagRange)
	for _, d := range Diagnostics(cp, reader) {
		if d.StartLine != d.EndLine {
			t.Fatalf("diagnostic %+v spans multiple lines, fixture expects single-line diagnostics", d)
		}
		if _, exists := ranges[int(d.StartLine)]; exists {
			t.Fatalf("line %d has more than one diagnostic, fixture expects exactly one per line", d.StartLine+1)
		}
		ranges[int(d.StartLine)] = diagRange{startCol: d.StartCol, endCol: d.EndCol, msg: d.Message}
	}
	return ranges
}
