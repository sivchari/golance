package xref

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// writeGenericsRefFixture writes a self-contained module reproducing the
// production report this fixture is modeled on (connectrpc.com/connect's
// Request[T]/Response[T].Msg shape, without depending on connectrpc
// itself): a box package declaring an instantiable Box[T] with a field and
// both a value- and a pointer-receiver method, plus an embedded generic
// struct (Wrapper), and three separate importing packages each selecting
// .Msg or calling those methods on Box[Concrete] (or, for one call, on
// Box[int] -- see PointerMethodUseB's doc) -- exercising symbol identity
// across several files and several distinct instantiations of the same
// generic declaration.
func writeGenericsRefFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/genrefs\n\ngo 1.23\n")
	writeTestFile(t, dir, "box/box.go", `package box

// Box wraps a value of type T, mirroring connectrpc.com/connect's
// Request[T]/Response[T] shape (a Msg *T field) that motivated this
// fixture.
type Box[T any] struct {
	Msg *T
}

// ValueDescribe is a value-receiver method on the generic Box type.
func (b Box[T]) ValueDescribe() string { return "box" }

// PointerDescribe is a pointer-receiver method on the generic Box type.
func (b *Box[T]) PointerDescribe() string { return "box" }

// Wrapper embeds an instantiated Box, for exercising a field and a method
// promoted from an embedded instantiated generic struct.
type Wrapper struct {
	Box[int]
}
`)
	writeTestFile(t, dir, "concrete/concrete.go", `package concrete

// Concrete is the type argument box.Box is instantiated with across usea
// and useb below.
type Concrete struct {
	Name string
}
`)
	writeTestFile(t, dir, "usea/usea1.go", `package usea

import (
	"example.com/genrefs/box"
	"example.com/genrefs/concrete"
)

// FieldUseA exercises the Msg field access on an instantiated Box[Concrete].
func FieldUseA() *concrete.Concrete {
	b := box.Box[concrete.Concrete]{}
	return b.Msg
}
`)
	writeTestFile(t, dir, "usea/usea2.go", `package usea

import (
	"example.com/genrefs/box"
	"example.com/genrefs/concrete"
)

// ValueMethodUseA exercises a value-receiver method call on an instantiated
// Box[Concrete].
func ValueMethodUseA() string {
	b := box.Box[concrete.Concrete]{}
	return b.ValueDescribe()
}
`)
	writeTestFile(t, dir, "usea/usea3.go", `package usea

import (
	"example.com/genrefs/box"
	"example.com/genrefs/concrete"
)

// PointerMethodUseA exercises a pointer-receiver method call on an
// instantiated Box[Concrete].
func PointerMethodUseA() string {
	b := &box.Box[concrete.Concrete]{}
	return b.PointerDescribe()
}
`)
	writeTestFile(t, dir, "useb/useb1.go", `package useb

import (
	"example.com/genrefs/box"
	"example.com/genrefs/concrete"
)

// FieldUseB exercises the Msg field access on an instantiated Box[Concrete]
// from a second importing package.
func FieldUseB() *concrete.Concrete {
	b := box.Box[concrete.Concrete]{}
	return b.Msg
}
`)
	writeTestFile(t, dir, "useb/useb2.go", `package useb

import "example.com/genrefs/box"

// PointerMethodUseB exercises a pointer-receiver method call on an
// instantiated Box[int] -- a different type argument than usea's own
// calls, pinning that symbol identity is stable across distinct
// instantiations of the same generic declaration, not just repeated with
// the same type argument.
func PointerMethodUseB() string {
	b := &box.Box[int]{}
	return b.PointerDescribe()
}
`)
	writeTestFile(t, dir, "usec/usec1.go", `package usec

import "example.com/genrefs/box"

// WrapperFieldUseC exercises a field promoted from an embedded
// instantiated generic struct (Wrapper embeds Box[int]).
func WrapperFieldUseC() *int {
	var w box.Wrapper
	return w.Msg
}

// WrapperMethodUseC exercises a method promoted from an embedded
// instantiated generic struct.
func WrapperMethodUseC() string {
	var w box.Wrapper
	return w.ValueDescribe()
}
`)
	return dir
}

// genericsRefCases enumerates, for each of Box's field and two methods,
// every use site's file (relative to the fixture module root) that
// references it -- see writeGenericsRefFixture's doc for what each
// function does.
var genericsRefCases = []struct {
	name  string
	files []string
}{
	{name: "Msg", files: []string{"usea/usea1.go", "useb/useb1.go", "usec/usec1.go"}},
	{name: "ValueDescribe", files: []string{"usea/usea2.go", "usec/usec1.go"}},
	{name: "PointerDescribe", files: []string{"usea/usea3.go", "useb/useb2.go"}},
}

// genericsRefFile resolves rel (as used in genericsRefCases, e.g.
// "usea/usea1.go") to its absolute path via snap, the same lookup every
// other xref test uses (see goFile's doc for why this -- not a plain
// filepath.Join -- is required: go/packages may canonicalize a path
// differently than a naive join, e.g. across a macOS /tmp symlink).
func genericsRefFile(t *testing.T, snap *graph.Snapshot, rel string) string {
	t.Helper()
	pkgPath := "example.com/genrefs/" + filepath.Dir(rel)
	return goFile(t, snap, pkgPath, filepath.Base(rel))
}

// wantGenericsRefLocations builds the expected, sorted Location set for
// tc: one location per file in tc.files, at that file's own (single, by
// fixture construction) occurrence of tc.name.
func wantGenericsRefLocations(t *testing.T, snap *graph.Snapshot, tc struct {
	name  string
	files []string
}) []Location {
	t.Helper()
	locs := make([]Location, 0, len(tc.files))
	for _, rel := range tc.files {
		file := genericsRefFile(t, snap, rel)
		line, col := identOccurrence(t, file, tc.name)
		l, c, err := toUint32Pos(line, col)
		if err != nil {
			t.Fatalf("toUint32Pos: %v", err)
		}
		locs = append(locs, Location{File: file, Line: l, Col: c, EndCol: c + u32len(len(tc.name))})
	}
	sortLocations(locs)
	return locs
}

// assertSameLocations fails the test unless got and want -- both sorted by
// sortLocations first -- name exactly the same set of locations: neither
// missing an expected use site nor including an unexpected extra one.
func assertSameLocations(t *testing.T, label string, got, want []Location) {
	t.Helper()
	gotSorted := append([]Location(nil), got...)
	sortLocations(gotSorted)
	if !reflect.DeepEqual(gotSorted, want) {
		t.Errorf("%s = %+v, want %+v", label, gotSorted, want)
	}
}

// TestReferences_InstantiatedGenericField_CrossPackage is the red-then-green
// regression test for facts extraction's symbol-identity bug: a field or
// method reached through an INSTANTIATED generic type
// (box.Box[concrete.Concrete], or the Wrapper struct embedding box.Box[int])
// is a synthetic go/types object distinct from its origin declaration (see
// depcheck.OriginObject's doc, added for the equivalent
// textDocument/definition bug this fixes the textDocument/references
// counterpart of). Before the fix, addRef's symbolID computed for such a
// reference could not find an objectpath for the synthetic object and fell
// back to a position-derived SymbolID string, which never matches the
// origin-based SymbolID addDef recorded for the same field/method's own
// declaration -- so every use through an instantiated generic type was
// silently absent from References' result, regardless of which file or
// package it came from.
//
// This asserts, for every case in genericsRefCases, that References run
// from BOTH the declaration and every individual use site returns the
// exact same complete set of locations, and that Definition run from every
// use site agrees on the declaration's own position.
func TestReferences_InstantiatedGenericField_CrossPackage(t *testing.T) {
	dir := writeGenericsRefFixture(t)
	r, snap := newResolverForDir(t, dir)
	boxFile := goFile(t, snap, "example.com/genrefs/box", "box.go")

	for _, tc := range genericsRefCases {
		t.Run(tc.name, func(t *testing.T) {
			declLine, declCol := identOccurrence(t, boxFile, tc.name)
			want := wantGenericsRefLocations(t, snap, tc)

			gotFromDecl, err := r.References(context.Background(), boxFile, declLine, declCol, false)
			if err != nil {
				t.Fatalf("References from declaration: %v", err)
			}
			assertSameLocations(t, "References from declaration", gotFromDecl, want)

			for _, rel := range tc.files {
				file := genericsRefFile(t, snap, rel)
				line, col := identOccurrence(t, file, tc.name)

				gotFromUse, err := r.References(context.Background(), file, line, col, false)
				if err != nil {
					t.Fatalf("References from %s: %v", rel, err)
				}
				assertSameLocations(t, fmt.Sprintf("References from %s", rel), gotFromUse, want)

				defLocs, err := r.Definition(context.Background(), file, line, col)
				if err != nil {
					t.Fatalf("Definition from %s: %v", rel, err)
				}
				if len(defLocs) != 1 || defLocs[0].File != boxFile || int(defLocs[0].Line) != declLine || int(defLocs[0].Col) != declCol {
					t.Errorf("Definition from %s = %+v, want {%s %d %d}", rel, defLocs, boxFile, declLine, declCol)
				}
			}
		})
	}
}

// goplsRefLocation is one location parsed from gopls's "references"
// subcommand output.
type goplsRefLocation struct {
	File string
	Line int
	Col  int
}

// goplsRefLineRE matches one full line of gopls's "references" output:
// "file:line:col" or "file:line:col-endcol" (no trailing description, unlike
// "definition"'s output -- see langfeat's goplsLocationRE for that
// counterpart).
var goplsRefLineRE = regexp.MustCompile(`^(.+):(\d+):(\d+)(?:-\d+)?$`)

// runGoplsReferences runs `gopls references [-d] <pos>` with cwd set to
// dir. GOCACHE and HOME are both redirected to scratch directories under
// gocacheHome (gopls's own build graph load needs a writable GOCACHE, and
// gopls's separate on-disk query cache lives under os.UserCacheDir(), which
// follows $HOME -- without this override every run shares (and races on)
// the ambient environment's real gopls cache, and a sandboxed environment
// that only permits writes to already-existing cache entries can fail
// outright the first time a never-before-seen fixture path is queried).
// Only stdout is parsed for location lines: gopls logs its own diagnostics
// (including any cache-write failure, which it degrades past rather than
// failing the query over) to stderr, so CombinedOutput would otherwise
// interleave log lines with real location lines and break the line-based
// parser below. Empty output is a valid, zero-length result -- not an
// error -- since that is exactly the documented gopls limitation
// TestReferences_InstantiatedGenericField_GoplsParity exercises for the Msg
// field case.
func runGoplsReferences(t *testing.T, goplsPath, dir, gocacheHome, pos string, includeDecl bool) []goplsRefLocation {
	t.Helper()
	args := []string{"references"}
	if includeDecl {
		args = append(args, "-d")
	}
	args = append(args, pos)
	cmd := exec.Command(goplsPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(gocacheHome, "gocache"), "HOME="+gocacheHome)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gopls references %s: %v\nstderr:\n%s", pos, err, stderr.String())
	}
	var locs []goplsRefLocation
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		m := goplsRefLineRE.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("could not parse gopls references output line: %q (full stdout: %q, stderr: %q)", line, out, stderr.String())
		}
		lineNum, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("parse line %q: %v", m[2], err)
		}
		col, err := strconv.Atoi(m[3])
		if err != nil {
			t.Fatalf("parse col %q: %v", m[3], err)
		}
		locs = append(locs, goplsRefLocation{File: canonicalPath(m[1]), Line: lineNum, Col: col})
	}
	return locs
}

// canonicalPath resolves path's symlinks (e.g. macOS's /tmp -> /private/tmp)
// before cleaning it, so a fixture path golance reports (from go/packages,
// which does not always resolve that particular symlink) and the identical
// path gopls's own CLI reports (which does) compare equal instead of
// differing only in which of the two equivalent forms each tool happened to
// print. Falls back to a plain Clean if the path cannot be resolved (should
// not happen for a real fixture file both tools just successfully read).
func canonicalPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// goplsPosArg formats file:line:col for gopls's CLI, relative to dir (as
// gopls's own examples do).
func goplsPosArg(dir, file string, line, col int) string {
	rel, err := filepath.Rel(dir, file)
	if err != nil {
		rel = file
	}
	return fmt.Sprintf("%s:%d:%d", filepath.ToSlash(rel), line, col)
}

// locationsToGopls converts golance Locations into the same
// (canonical-path, line, col) shape runGoplsReferences returns, for a
// direct comparison ignoring EndCol (gopls's own output sometimes omits
// the endcol suffix entirely -- see goplsRefLineRE -- so EndCol is not
// part of the comparison either direction).
func locationsToGopls(locs []Location) []goplsRefLocation {
	out := make([]goplsRefLocation, 0, len(locs))
	for _, l := range locs {
		out = append(out, goplsRefLocation{File: canonicalPath(l.File), Line: int(l.Line), Col: int(l.Col)})
	}
	return out
}

func sortGoplsLocations(locs []goplsRefLocation) {
	sort.Slice(locs, func(i, j int) bool {
		a, b := locs[i], locs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
}

// TestReferences_InstantiatedGenericMethods_GoplsParity checks golance's
// References for the two METHOD rows (ValueDescribe, PointerDescribe)
// against gopls v0.23.0's own "references -d" resolution of the identical
// declaration-site query, over the exact same fixture -- gopls quality is
// the bar this fix is held to for these two rows. Skipped if gopls is not
// on PATH.
//
// The FIELD row (Msg) is deliberately NOT compared for exact equality
// here: see TestReferences_InstantiatedGenericField_GoplsParity, which
// documents why gopls itself -- not just golance before this fix -- fails
// to find any use of a struct field reached through an instantiated
// generic type.
func TestReferences_InstantiatedGenericMethods_GoplsParity(t *testing.T) {
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := writeGenericsRefFixture(t)
	r, snap := newResolverForDir(t, dir)
	boxFile := goFile(t, snap, "example.com/genrefs/box", "box.go")
	gocacheHome := t.TempDir()

	for _, tc := range genericsRefCases {
		if tc.name == "Msg" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			declLine, declCol := identOccurrence(t, boxFile, tc.name)

			got, err := r.References(context.Background(), boxFile, declLine, declCol, true)
			if err != nil {
				t.Fatalf("References: %v", err)
			}
			gotNorm := locationsToGopls(got)
			sortGoplsLocations(gotNorm)

			pos := goplsPosArg(dir, boxFile, declLine, declCol)
			want := runGoplsReferences(t, goplsPath, dir, gocacheHome, pos, true)
			sortGoplsLocations(want)

			if !reflect.DeepEqual(gotNorm, want) {
				t.Errorf("golance References (normalized) = %+v, gopls references -d %s = %+v", gotNorm, pos, want)
			}
		})
	}
}

// TestReferences_InstantiatedGenericField_GoplsParity documents and pins an
// empirically confirmed gopls v0.23.0 limitation, rather than asserting
// exact parity: gopls's own "references" subcommand, run either from the
// Msg field's declaration or from any one of its use sites across usea,
// useb, and usec, finds NO uses of a struct field reached through an
// instantiated generic type -- not even the query site itself. This was
// verified directly against this exact fixture (and independently against
// the pre-existing internal/langfeat/testdata/module/genericdep+genericuse
// fixture) before writing this test: `gopls references -d` on the Msg
// field's declaration returns only the declaration itself; without -d, the
// output is empty.
//
// This is a real, upstream limitation of gopls's own reference-finding for
// this exact shape -- not a golance shortfall this package could match by
// loosening its own completeness. Once TestReferences_InstantiatedGenericField_CrossPackage's
// fix lands, golance's own References for this row is a strict superset of
// gopls's: golance finds every one of the three real use sites gopls
// itself misses. If a future gopls release fixes this, the "want empty"
// assertions below will start failing, which is the intended signal to
// revisit this test rather than silently keep asserting the wrong thing.
func TestReferences_InstantiatedGenericField_GoplsParity(t *testing.T) {
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := writeGenericsRefFixture(t)
	_, snap := newResolverForDir(t, dir)
	boxFile := goFile(t, snap, "example.com/genrefs/box", "box.go")
	gocacheHome := t.TempDir()

	declLine, declCol := identOccurrence(t, boxFile, "Msg")
	pos := goplsPosArg(dir, boxFile, declLine, declCol)

	withoutDecl := runGoplsReferences(t, goplsPath, dir, gocacheHome, pos, false)
	if len(withoutDecl) != 0 {
		t.Errorf("gopls references %s = %+v, want empty -- gopls appears to have fixed the generic-field limitation this test documents; see its doc comment", pos, withoutDecl)
	}

	withDecl := runGoplsReferences(t, goplsPath, dir, gocacheHome, pos, true)
	if len(withDecl) != 1 || withDecl[0].Line != declLine || withDecl[0].Col != declCol {
		t.Errorf("gopls references -d %s = %+v, want exactly the declaration itself at line %d col %d", pos, withDecl, declLine, declCol)
	}
}
