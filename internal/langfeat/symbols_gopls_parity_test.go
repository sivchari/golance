package langfeat_test

// This file regression-tests two documentSymbol fixes made against a real
// gopls v0.23.0 oracle (see ./audit-informational.md finding #9 and #10 at
// the repo root): DocumentSymbols (symbols.go) now nests a struct's fields
// and an interface's method set/embedded types under their declaring
// type, and distinguishes an interface (SymbolInterface) from a struct or
// plain defined type (SymbolType) instead of collapsing every type
// declaration into one bucket. `gopls symbols <file>` is used as the
// ground truth for which children a type should have, their kind, and
// their position.

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// goplsSymbol is one parsed line of `gopls symbols`'s flat, tab-indented
// outline: e.g. "\tName Field 36:2-36:6" parses to {name: "Name", kind:
// "Field", line: 36, col: 2}, depth 1.
type goplsSymbol struct {
	depth int
	name  string
	kind  string
	line  int
	col   int
}

// goplsSymbolLineRE matches a `gopls symbols` line after its leading tabs
// are stripped: capturing the name (which may itself contain spaces, e.g.
// a generic interface's type-set element `~int | ~int32`), the kind, and
// the start line:col of its "line:col-line:col" range.
var goplsSymbolLineRE = regexp.MustCompile(`^(.*)\s+(Function|Method|Interface|Struct|Class|Variable|Constant|Field)\s+(\d+):(\d+)-\d+:\d+$`)

// runGoplsSymbols runs `gopls symbols path` and parses its output into
// goplsSymbol entries, preserving source order and tab-nesting depth.
func runGoplsSymbols(t *testing.T, path string) []goplsSymbol {
	t.Helper()
	args := []string{"symbols"}
	args = append(args, path)
	cmd := exec.Command("gopls", args...)
	cmd.Env = append(os.Environ(), "GOCACHE="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gopls symbols %s: %v", path, err)
	}

	var syms []goplsSymbol
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		depth := 0
		for depth < len(line) && line[depth] == '\t' {
			depth++
		}
		m := goplsSymbolLineRE.FindStringSubmatch(line[depth:])
		if m == nil {
			t.Fatalf("unparseable gopls symbols line %q", line)
		}
		lineNum, err := strconv.Atoi(m[3])
		if err != nil {
			t.Fatalf("parse line number in %q: %v", line, err)
		}
		col, err := strconv.Atoi(m[4])
		if err != nil {
			t.Fatalf("parse column in %q: %v", line, err)
		}
		syms = append(syms, goplsSymbol{depth: depth, name: m[1], kind: m[2], line: lineNum, col: col})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan gopls symbols output: %v", err)
	}
	return syms
}

// goplsNode is a top-level (depth 0) goplsSymbol together with the
// depth-1 entries gopls nested under it.
type goplsNode struct {
	sym      goplsSymbol
	children []goplsSymbol
}

// groupGoplsTree groups flat's depth-1+ entries under the nearest
// preceding depth-0 entry, mirroring how `gopls symbols`'s tab indentation
// expresses nesting for the shapes this test's fixtures produce (at most
// one level deep).
func groupGoplsTree(flat []goplsSymbol) []goplsNode {
	var nodes []goplsNode
	for _, s := range flat {
		if s.depth == 0 {
			nodes = append(nodes, goplsNode{sym: s})
			continue
		}
		if len(nodes) == 0 {
			continue
		}
		last := &nodes[len(nodes)-1]
		last.children = append(last.children, s)
	}
	return nodes
}

// goplsTypeKindToSymbolKind maps a top-level `gopls symbols` type kind to
// the langfeat.SymbolKind DocumentSymbols is expected to report: gopls
// distinguishes a plain defined type (Class) from a struct (Struct), a
// finer split than the facts index (internal/index's KindType) carries for
// workspace/symbol, so DocumentSymbols intentionally keeps Class and
// Struct in the same SymbolType bucket -- see kinds.go's documentSymbolKind
// doc -- while still splitting out Interface. ok is false for any other
// gopls kind (a func/var/const, not a type declaration).
func goplsTypeKindToSymbolKind(k string) (langfeat.SymbolKind, bool) {
	switch k {
	case "Interface":
		return langfeat.SymbolInterface, true
	case "Struct", "Class":
		return langfeat.SymbolType, true
	default:
		return 0, false
	}
}

// goplsChildKindToSymbolKind maps a `gopls symbols` child kind (a struct
// field, an interface method, or an interface's embedded
// type/type-set element) to its langfeat.SymbolKind equivalent.
func goplsChildKindToSymbolKind(k string) langfeat.SymbolKind {
	if k == "Method" {
		return langfeat.SymbolMethod
	}
	return langfeat.SymbolField
}

// nonMethodChildren drops SymbolMethod entries from children: a struct's
// Children can also hold a same-file, matching-receiver method nested by
// appendFuncSymbol (a separate, pre-existing, already-tested golance
// convention -- see TestDocumentSymbols_Hierarchy -- that gopls does not
// follow; gopls always lists such a method at the top level, e.g.
// "(*baseServer).Name Method"). Comparing against gopls's own field-only
// child list requires filtering that convention's output back out first.
func nonMethodChildren(children []langfeat.Symbol) []langfeat.Symbol {
	out := make([]langfeat.Symbol, 0, len(children))
	for _, c := range children {
		if c.Kind != langfeat.SymbolMethod {
			out = append(out, c)
		}
	}
	return out
}

// TestDocumentSymbols_GoplsParity drives a real gopls v0.23.0 (skipped if
// not on PATH) and langfeat.DocumentSymbols over the same fixture files,
// and asserts every gopls-reported type declaration's kind and child set
// (name, kind, start position) match -- see this file's own doc comment
// for the two fixes this pins as fixed. Covers structs with fields
// (Config, Server, baseServer), interfaces with methods (Reader, Writer)
// and an embedded interface (ReadWriter), a struct embedding another
// struct (Server embeds baseServer), value and pointer receiver methods
// (errZero.Error, (*baseServer).Name, (*Request[T]).Any), generic types
// (Request[T], Response[T], Numeric's type-set constraint), and a const
// block (consts.go's Level/LevelDebug...).
func TestDocumentSymbols_GoplsParity(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	for _, file := range []string{"iface.go", "wrapper.go", "funcs.go", "consts.go"} {
		t.Run(file, func(t *testing.T) {
			reader := overlay.New()
			cp, path := newCheckedPackage(t, reader, "auditfeat", file)
			text, err := reader.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			syms, err := langfeat.DocumentSymbols(cp, path)
			if err != nil {
				t.Fatalf("DocumentSymbols: %v", err)
			}

			for _, node := range groupGoplsTree(runGoplsSymbols(t, path)) {
				wantKind, isType := goplsTypeKindToSymbolKind(node.sym.kind)
				if !isType {
					continue // a func/var/const declaration; unaffected by this fix
				}

				got := findSymbol(syms, node.sym.name)
				if got == nil {
					t.Fatalf("DocumentSymbols has no top-level symbol %q (gopls reports it as %s)", node.sym.name, node.sym.kind)
				}
				if got.Kind != wantKind {
					t.Errorf("%s.Kind = %v, want %v (gopls kind %q)", node.sym.name, got.Kind, wantKind, node.sym.kind)
				}

				gotChildren := got.Children
				if node.sym.kind != "Interface" {
					gotChildren = nonMethodChildren(gotChildren)
				}
				assertChildrenMatch(t, text, node.sym.name, gotChildren, node.children)
			}
		})
	}
}

// assertChildrenMatch compares got (golance's DocumentSymbols children for
// parent, already filtered to exclude any pre-existing, out-of-scope
// receiver-method nesting) against want (gopls's own children for the same
// parent), by name, kind, and start position -- order-independent, since
// neither side's child order is part of this fix's contract.
func assertChildrenMatch(t *testing.T, text []byte, parent string, got []langfeat.Symbol, want []goplsSymbol) {
	t.Helper()

	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
	sort.Slice(want, func(i, j int) bool { return want[i].name < want[j].name })

	gotNames := make([]string, len(got))
	for i, s := range got {
		gotNames[i] = s.Name
	}
	wantNames := make([]string, len(want))
	for i, s := range want {
		wantNames[i] = s.name
	}
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("%s children = %v, want %v (gopls)", parent, gotNames, wantNames)
	}

	for i, w := range want {
		g := got[i]
		wantKind := goplsChildKindToSymbolKind(w.kind)
		if g.Kind != wantKind {
			t.Errorf("%s.%s.Kind = %v, want %v (gopls kind %q)", parent, w.name, g.Kind, wantKind, w.kind)
		}
		gotLine, gotCol := lineCol(text, g.Range.StartOffset)
		if gotLine != w.line || gotCol != w.col {
			t.Errorf("%s.%s range start = %d:%d, want %d:%d (gopls)", parent, w.name, gotLine, gotCol, w.line, w.col)
		}
	}
}
