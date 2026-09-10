package server

// This file is the golance-vs-gopls navigation parity audit's permanent
// regression suite (see ./audit-navigation.md at the repo root for the
// full findings report). It drives golance's handlers directly through
// newTestServer, the same in-process pattern handlers_xref_test.go and
// handlers_typehierarchy_test.go already use, and compares every result
// against gopls v0.23.0 -- the CLI for definition/references/
// implementation/call_hierarchy/prepare_rename/rename, and, since gopls's
// CLI has no subcommand for typeDefinition or typeHierarchy, a live `gopls
// serve` driven over LSP stdio (navaudit_gopls_helper_test.go).
//
// Every test in this file requireGopls(t)-skips when no gopls binary is on
// PATH, so this suite never fails a machine that simply lacks the oracle.
//
// The fixture module lives under testdata/module/navaudit/ -- new packages
// added to the existing shared testdata/module (see newTestServer's doc),
// never editing any existing file there.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// navPos names one cursor position in the navaudit fixture: the
// occurrence-th (1-based, source order) identifier named ident in file
// (relative to testdata/module/navaudit/). label documents which shape from
// the audit's mandate (generics, interfaces, cross-package, test packages,
// generated-code-like wrappers, shadowing, ...) the position exercises.
type navPos struct {
	label string
	file  string
	ident string
	occ   int
}

// navPositions is the shared position table definition, typeDefinition,
// references, and prepareRename are all run against (each targets a
// different LSP feature, but "is there a renameable/resolvable symbol
// here" is a meaningful question at every one of these positions).
var navPositions = []navPos{
	{"generic struct decl", "generics/generics.go", "Box", 1},
	{"generic value-receiver method's receiver type", "generics/generics.go", "Box", 2},
	{"generic field use in method body", "generics/generics.go", "Value", 2},
	{"generic func, explicit type args", "generics/generics.go", "MapSlice", 2},
	{"generic func, inferred type args", "generics/generics.go", "MapSlice", 3},
	{"generic constraint method call (T.Add)", "generics/generics.go", "Add", 3},
	{"nested instantiation alias decl", "generics/generics.go", "NestedBox", 1},
	{"nested instantiation var use", "generics/generics.go", "nested", 2},
	{"embedded generic struct instantiation use", "generics/generics.go", "Container", 2},
	{"embedded interface decl", "iface/iface.go", "Greeter", 1},
	{"interface method call promoted through embedding (g.Speak())", "iface/iface.go", "Speak", 3},
	{"interface method call (g.Name())", "iface/iface.go", "Name", 3},
	{"cross-package interface reference", "impl/impl.go", "Speaker", 1},
	{"pointer-receiver-only interface satisfaction decl", "impl/impl.go", "PtrSpeaker", 1},
	{"connectrpc-like generic wrapper decl", "wrapper/wrapper.go", "Request", 1},
	{"instantiated generic wrapper field access", "wrapper/wrapper.go", "Name", 3},
	{"aliased import type reference", "reexport/reexport.go", "Box", 1},
	{"dot-imported symbol use", "reexport/reexport.go", "Speaker", 1},
	{"shadowed identifier resolves to outer decl", "misc/misc.go", "x", 4},
	{"method value (c.Inc, not called)", "misc/misc.go", "Inc", 2},
	{"method promoted through two embedding levels", "misc/misc.go", "M", 2},
	{"labeled continue target", "misc/misc.go", "Outer", 2},
	{"anonymous struct field use", "misc/misc.go", "X", 2},
	{"in-package _test.go symbol use", "testpkg/testpkg_test.go", "Symbol", 1},
	{"external _test package symbol use", "testpkg/testpkg_ext_test.go", "Symbol", 1},
}

// knownGaps records every "feature/label" case this suite currently
// expects to disagree with gopls on, each backed by a row in
// ./audit-navigation.md's findings table. reportMismatch logs (not fails)
// these instead of erroring, so the suite stays a real regression guard for
// every case that already matches gopls -- catching any new drift there --
// without either silently swallowing a known gap (a plain t.Skip would) or
// leaving today's fixed set of known gaps permanently red, which would
// hide a genuinely new regression among the expected noise. As each gap
// closes in the coordinated fix wave the audit's own doc calls for, delete
// its entry here so this suite starts enforcing it too.
var knownGaps = map[string]bool{
	// gopls v0.23.0 cannot find references to a generic type's field when
	// reached only through a plain (non-generic) type alias of an
	// instantiation: golance's References on generics.Box[T].Value finds 10
	// locations, including reexport.go:15 (navg.IntBox{Value: 1}) and
	// reexport.go:27 (ReBox{Value: 3}); gopls finds only the other 8,
	// omitting both. IntBox and ReBox are both plain aliases of Box[int], so
	// a Value: key in either composite literal genuinely does set Box[int]'s
	// own field -- golance's broader answer is the correct one, gopls's
	// shorter one is the gap. Kept allowlisted rather than promoted to an
	// enforced case: the suite treats gopls as its oracle, and there is
	// nothing to change on golance's side to make the two agree.
	"References/generic field use in method body": true,
}

// reportMismatch logs a golance-vs-gopls disagreement for feature/label:
// t.Errorf (a real regression) unless it is a knownGaps entry, in which
// case t.Logf so -v output still shows it without failing the suite.
func reportMismatchf(t *testing.T, feature, label, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if knownGaps[feature+"/"+label] {
		t.Logf("KNOWN GAP (see audit-navigation.md): %s", msg)
		return
	}
	t.Errorf("%s", msg)
}

func navFile(root, rel string) string {
	return filepath.Join(root, "navaudit", filepath.FromSlash(rel))
}

func navPosition(t *testing.T, root string, p navPos) (file string, pos protocol.Position) {
	t.Helper()
	file = navFile(root, p.file)
	data := mustReadFile(t, file)
	return file, identPositionIn(t, file, data, p.ident, p.occ)
}

func mustLocationSlice(t *testing.T, result any) protocol.LocationSlice {
	t.Helper()
	locs, ok := result.(protocol.LocationSlice)
	if !ok {
		t.Fatalf("result = %#v (%T), want protocol.LocationSlice", result, result)
	}
	return locs
}

// --- textDocument/definition ---

func TestNavAudit_Definition(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()

	for _, p := range navPositions {
		t.Run(p.label, func(t *testing.T) {
			file, pos := navPosition(t, root, p)

			result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
					Position:     pos,
				},
			}))
			if err != nil {
				t.Fatalf("golance handleDefinition: %v", err)
			}
			golanceLocs := locsFromLSP(mustLocationSlice(t, result))

			goplsOut, gerr := runGoplsDefinitionJSON(t, cacheDir, root, goplsPosArg(t, root, file, pos))
			goplsLocs, perr := parseGoplsJSONSpans(t, goplsOut)
			if gerr != nil && perr != nil {
				t.Fatalf("gopls definition: %v (output: %s)", gerr, goplsOut)
			}

			if !locsEqualSet(golanceLocs, goplsLocs) {
				reportMismatchf(t, "Definition", p.label, "mismatch at %s %q occurrence %d:\n golance = [%s]\n gopls   = [%s]",
					p.file, p.ident, p.occ, locsString(golanceLocs), locsString(goplsLocs))
			}
		})
	}
}

// --- textDocument/typeDefinition (LSP-driven: no gopls CLI subcommand) ---

func TestNavAudit_TypeDefinition(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()
	g := startGoplsLSP(t, cacheDir, root)
	for _, f := range navauditFiles(t, root) {
		g.didOpen(t, f)
	}

	for _, p := range navPositions {
		t.Run(p.label, func(t *testing.T) {
			file, pos := navPosition(t, root, p)

			result, err := s.handleTypeDefinition(context.Background(), mustMarshal(t, &protocol.TypeDefinitionParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
					Position:     pos,
				},
			}))
			if err != nil {
				t.Fatalf("golance handleTypeDefinition: %v", err)
			}
			golanceLocs := locsFromLSP(mustLocationSlice(t, result))

			goplsLocs, gerr := g.typeDefinitionOrErr(t, file, pos)
			if gerr != nil {
				// gopls answers a genuine protocol error for some
				// positions (e.g. a zero/multi-result function
				// identifier) where golance always answers an empty
				// result instead -- see ./audit-navigation.md. Neither
				// side crashed, but there is nothing to diff, so this
				// is recorded as unverifiable rather than compared.
				t.Logf("UNVERIFIABLE at %s %q occurrence %d: gopls errored: %v (golance = [%s])",
					p.file, p.ident, p.occ, gerr, locsString(golanceLocs))
				return
			}

			if !locsEqualSet(golanceLocs, goplsLocs) {
				reportMismatchf(t, "TypeDefinition", p.label, "mismatch at %s %q occurrence %d:\n golance = [%s]\n gopls   = [%s]",
					p.file, p.ident, p.occ, locsString(golanceLocs), locsString(goplsLocs))
			}
		})
	}
}

// navauditFiles lists every .go file (including _test.go) under
// testdata/module/navaudit, for the LSP driver's didOpen calls.
func navauditFiles(t *testing.T, root string) []string {
	t.Helper()
	base := filepath.Join(root, "navaudit")
	var out []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", base, err)
	}
	sort.Strings(out)
	return out
}

// --- textDocument/references ---

func TestNavAudit_References(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()

	for _, p := range navPositions {
		t.Run(p.label, func(t *testing.T) {
			file, pos := navPosition(t, root, p)

			result, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
					Position:     pos,
				},
				Context: protocol.ReferenceContext{IncludeDeclaration: false},
			}))
			if err != nil {
				t.Fatalf("golance handleReferences: %v", err)
			}
			golanceLocs := locsFromLSP(mustLocationSlice(t, result))

			goplsOut, _ := runGopls(t, cacheDir, root, "references", goplsPosArg(t, root, file, pos))
			goplsLocs := parseSpanLines(goplsOut)

			if !locsEqualSet(golanceLocs, goplsLocs) {
				reportMismatchf(t, "References", p.label, "mismatch at %s %q occurrence %d:\n golance = [%s]\n gopls   = [%s]",
					p.file, p.ident, p.occ, locsString(golanceLocs), locsString(goplsLocs))
			}
		})
	}
}

// --- textDocument/prepareRename ---

func rangeToNavLoc(file string, r protocol.Range) navLoc {
	return navLoc{
		file:    file,
		line:    int(r.Start.Line),
		col:     int(r.Start.Character),
		endLine: int(r.End.Line),
		endCol:  int(r.End.Character),
	}
}

func TestNavAudit_PrepareRename(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()

	for _, p := range navPositions {
		t.Run(p.label, func(t *testing.T) {
			file, pos := navPosition(t, root, p)

			result, err := s.handlePrepareRename(context.Background(), mustMarshal(t, &protocol.PrepareRenameParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
					Position:     pos,
				},
			}))
			if err != nil {
				t.Fatalf("golance handlePrepareRename: %v", err)
			}

			goplsOut, gerr := runGopls(t, cacheDir, root, "prepare_rename", goplsPosArg(t, root, file, pos))
			goplsLocs := parseSpanLines(goplsOut)

			rng, golanceOK := result.(*protocol.Range)
			goplsOK := gerr == nil && len(goplsLocs) == 1

			if golanceOK != goplsOK {
				t.Errorf("renameable mismatch at %s %q occurrence %d: golance ok=%v, gopls ok=%v (gopls output: %q)",
					p.file, p.ident, p.occ, golanceOK, goplsOK, goplsOut)
				return
			}
			if !golanceOK {
				return
			}
			golanceLoc := rangeToNavLoc(file, *rng)
			if !locsEqualSet([]navLoc{golanceLoc}, goplsLocs) {
				t.Errorf("range mismatch at %s %q occurrence %d:\n golance = %s\n gopls   = %s",
					p.file, p.ident, p.occ, golanceLoc, goplsLocs[0])
			}
		})
	}
}
