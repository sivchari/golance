package server

// This file is the permanent regression suite for audit-navigation.md's
// finding 1 (definition/references from inside a workspace directory's
// external "_test" package answer empty/incomplete) and its rename
// sub-finding 2B (a rename covering a use from such a file leaves it
// stale, so the renamed package no longer compiles). See
// internal/index/testfiles.go's isExternalTestOfRoot doc for the fix these
// tests pin down: a directory's external "_test" package now gets its own
// facts-index unit, scheduled, indexed, and reindexed exactly like an
// ordinary root package (internal/index/scheduler.go's schedulableRoot).
//
// The fixture (testdata/module/exttestcheck/basepkg) is new, dedicated to
// this file rather than reusing testdata/module/navaudit/testpkg: Compute's
// doc comment deliberately does not start with its own name, so a rename
// here never also exercises gopls's separate "doc comment starts with the
// symbol's own name" convention-based rewrite (a distinct, out-of-scope gap
// -- see audit-navigation.md finding 2's Case A) alongside what this file
// actually tests, keeping a content-mismatch failure here unambiguous.
//
// gopls v0.23.0 is the oracle; every test here skips outright when no gopls
// binary is on PATH, matching navaudit_parity_test.go's own policy.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const exttestDir = "exttestcheck/basepkg"

// exttestFile returns the absolute path of name (a file base name) inside
// the fixture package's directory under root.
func exttestFile(root, name string) string {
	return filepath.Join(root, filepath.FromSlash(exttestDir), name)
}

// TestExternalTestPackage_DefinitionMatchesGopls verifies that
// textDocument/definition invoked from inside basepkg's external "_test"
// package file resolves to Compute's own declaration in basepkg.go --
// exactly what gopls answers -- rather than the empty result
// audit-navigation.md's finding 1 documented.
func TestExternalTestPackage_DefinitionMatchesGopls(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()

	file := exttestFile(root, "basepkg_ext_test.go")
	pos := identPositionIn(t, file, mustReadFile(t, file), "Compute", 1)

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
		t.Errorf("definition mismatch:\n golance = [%s]\n gopls   = [%s]", locsString(golanceLocs), locsString(goplsLocs))
	}
	if len(golanceLocs) != 1 {
		t.Fatalf("golance definition returned %d location(s), want 1: %s", len(golanceLocs), locsString(golanceLocs))
	}
	if want := exttestFile(root, "basepkg.go"); golanceLocs[0].file != want {
		t.Errorf("definition file = %s, want %s", golanceLocs[0].file, want)
	}
}

// TestExternalTestPackage_ReferencesMatchGopls verifies that
// textDocument/references on Compute's own declaration includes both the
// in-package _test.go call site and the external "_test" package's own
// call site, matching gopls, and that a references query issued FROM
// inside the external test file itself also resolves (rather than the
// facts-index "not part of any known package" miss finding 1 documented).
func TestExternalTestPackage_ReferencesMatchGopls(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()

	declFile := exttestFile(root, "basepkg.go")
	declPos := identPositionIn(t, declFile, mustReadFile(t, declFile), "Compute", 1)

	extFile := exttestFile(root, "basepkg_ext_test.go")
	extPos := identPositionIn(t, extFile, mustReadFile(t, extFile), "Compute", 1)

	cases := []struct {
		name string
		file string
		pos  protocol.Position
	}{
		{"from declaration", declFile, declPos},
		{"from external test file", extFile, extPos},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(tc.file)},
					Position:     tc.pos,
				},
				Context: protocol.ReferenceContext{IncludeDeclaration: false},
			}))
			if err != nil {
				t.Fatalf("golance handleReferences: %v", err)
			}
			golanceLocs := locsFromLSP(mustLocationSlice(t, result))

			goplsOut, _ := runGopls(t, cacheDir, root, goplsPosArg(t, root, tc.file, tc.pos))
			goplsLocs := parseSpanLines(goplsOut)

			if !locsEqualSet(golanceLocs, goplsLocs) {
				t.Errorf("references mismatch:\n golance = [%s]\n gopls   = [%s]", locsString(golanceLocs), locsString(goplsLocs))
			}

			found := false
			for _, l := range golanceLocs {
				if l.file == extFile {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("golance references did not include the external test package's own call site %s: %s", extFile, locsString(golanceLocs))
			}
		})
	}
}

// TestExternalTestPackageRename_CompilesAndMatchesGopls renames Compute,
// applies golance's WorkspaceEdit to a disposable copy of the fixture
// module, and asserts the result still builds -- the property that
// actually matters for audit-navigation.md's finding 2B: a rename that
// leaves the external test package referring to the old name no longer
// compiles. It also compares golance's touched-file set and content
// against gopls's own rename as an independent cross-check.
func TestExternalTestPackageRename_CompilesAndMatchesGopls(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)

	declFile := exttestFile(root, "basepkg.go")
	declPos := identPositionIn(t, declFile, mustReadFile(t, declFile), "Compute", 1)

	const newName = "Recompute"
	golanceByRel := golanceRenameEdits(t, s, root, declFile, declPos, newName)

	extRel := filepath.ToSlash(filepath.Join(exttestDir, "basepkg_ext_test.go"))
	if _, ok := golanceByRel[extRel]; !ok {
		t.Fatalf("golance rename did not touch the external test package file %s; touched: %v", extRel, relKeys(golanceByRel))
	}

	tmpRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	copyFixtureTree(t, root, tmpRoot)

	goplsByRel := goplsRenameEditsAt(t, tmpRoot, exttestFile(tmpRoot, "basepkg.go"), declPos, newName)
	p := navPos{label: "external test package rename fixture"}
	compareRenameResults(t, &p, golanceByRel, goplsByRel)

	for rel, content := range golanceByRel {
		if err := os.WriteFile(filepath.Join(tmpRoot, filepath.FromSlash(rel)), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	cacheDir := t.TempDir()
	cmd := exec.Command("go", "build", "./exttestcheck/...")
	cmd.Dir = tmpRoot
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(cacheDir, "gocache"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build after golance rename failed: %v\n%s", err, out)
	}
	cmd = exec.Command("go", "vet", "./exttestcheck/...")
	cmd.Dir = tmpRoot
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(cacheDir, "gocache"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet after golance rename failed: %v\n%s", err, out)
	}
}

// goplsRenameEditsAt runs `gopls rename -w -l` against tmpFile (already
// inside a disposable copy of the module rooted at tmpRoot -- see
// copyFixtureTree) and reads back every file it reports touching, keyed by
// path relative to tmpRoot (directly comparable to golanceRenameEdits'
// keys). Unlike navaudit_parity2_test.go's own goplsRenameEdits, this does
// not assume the fixture lives under a "navaudit/" subdirectory, so it
// works for this file's own testdata/module/exttestcheck fixture too.
func goplsRenameEditsAt(t *testing.T, tmpRoot, tmpFile string, pos protocol.Position, newName string) map[string]string {
	t.Helper()
	posArg := goplsPosArg(t, tmpRoot, tmpFile, pos)
	out, err := runGoplsRename(t, t.TempDir(), tmpRoot, posArg, newName)
	if err != nil {
		t.Fatalf("gopls rename: %v (%s)", err, out)
	}
	byRel := map[string]string{}
	for _, line := range splitNonEmptyLines(out) {
		rel, err := filepath.Rel(tmpRoot, line)
		if err != nil {
			t.Fatalf("filepath.Rel: %v", err)
		}
		data, err := os.ReadFile(filepath.Clean(line))
		if err != nil {
			t.Fatalf("read %s: %v", line, err)
		}
		byRel[filepath.ToSlash(rel)] = string(data)
	}
	return byRel
}
