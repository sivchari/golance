package server

// Continuation of navaudit_parity_test.go: implementation, call hierarchy,
// type hierarchy, and full rename parity cases. Split into a second file
// only to keep each file a manageable size; see navaudit_parity_test.go's
// doc for the suite's overall shape and ./audit-navigation.md for results.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// --- textDocument/implementation ---

func TestNavAudit_Implementation(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()

	cases := []navPos{
		{"interface decl (Speaker)", "iface/iface.go", "Speaker", 1},
		{"embedded interface decl (Greeter)", "iface/iface.go", "Greeter", 1},
		{"interface method decl (Speaker.Speak)", "iface/iface.go", "Speak", 1},
		{"concrete method decl, reverse direction (ValueSpeaker.Speak)", "impl/impl.go", "Speak", 1},
	}

	for _, p := range cases {
		t.Run(p.label, func(t *testing.T) {
			file, pos := navPosition(t, root, p)

			result, err := s.handleImplementation(context.Background(), mustMarshal(t, &protocol.ImplementationParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
					Position:     pos,
				},
			}))
			if err != nil {
				t.Fatalf("golance handleImplementation: %v", err)
			}
			golanceLocs := locsFromLSP(mustLocationSlice(t, result))

			goplsOut, _ := runGopls(t, cacheDir, root, "implementation", goplsPosArg(t, root, file, pos))
			goplsLocs := parseSpanLines(goplsOut)

			if !locsEqualSet(golanceLocs, goplsLocs) {
				reportMismatchf(t, "Implementation", p.label, "mismatch at %s %q occurrence %d:\n golance = [%s]\n gopls   = [%s]",
					p.file, p.ident, p.occ, locsString(golanceLocs), locsString(goplsLocs))
			}
		})
	}
}

// --- callHierarchy/incomingCalls, callHierarchy/outgoingCalls ---

func TestNavAudit_CallHierarchy(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	cacheDir := t.TempDir()

	cases := []navPos{
		{"outgoing calls, promoted interface methods (useGreeter)", "iface/iface.go", "useGreeter", 1},
		{"outgoing calls, single call (callWithNamed)", "iface/iface.go", "callWithNamed", 1},
		{"incoming calls (useGreeter, called by callWithNamed)", "iface/iface.go", "useGreeter", 1},
		{"incoming calls, method value call site (Counter.Inc)", "misc/misc.go", "Inc", 1},
	}

	for _, p := range cases {
		t.Run(p.label, func(t *testing.T) {
			file, pos := navPosition(t, root, p)

			prepResult, err := s.handlePrepareCallHierarchy(context.Background(), mustMarshal(t, &protocol.CallHierarchyPrepareParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
					Position:     pos,
				},
			}))
			if err != nil {
				t.Fatalf("golance handlePrepareCallHierarchy: %v", err)
			}
			items, ok := prepResult.([]protocol.CallHierarchyItem)
			if !ok || len(items) != 1 {
				t.Fatalf("handlePrepareCallHierarchy = %#v, want exactly one item", prepResult)
			}
			item := items[0]

			goplsOut, gerr := runGopls(t, cacheDir, root, "call_hierarchy", goplsPosArg(t, root, file, pos))
			if gerr != nil {
				t.Fatalf("gopls call_hierarchy: %v (%s)", gerr, goplsOut)
			}
			goplsCallers, goplsCallees := parseGoplsCallHierarchy(goplsOut)

			t.Run("outgoing", func(t *testing.T) {
				checkOutgoingCalls(t, s, &p, &item, goplsCallees)
			})
			t.Run("incoming", func(t *testing.T) {
				checkIncomingCalls(t, s, &p, &item, goplsCallers)
			})
		})
	}
}

// checkOutgoingCalls compares golance's handleOutgoingCalls result for item
// against goplsCallees (parsed from gopls's own call_hierarchy output),
// as an order-insensitive set of callee locations.
func checkOutgoingCalls(t *testing.T, s *Server, p *navPos, item *protocol.CallHierarchyItem, goplsCallees []callHierarchyEdge) {
	t.Helper()
	outResult, err := s.handleOutgoingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyOutgoingCallsParams{Item: *item}))
	if err != nil {
		t.Fatalf("golance handleOutgoingCalls: %v", err)
	}
	calls, ok := outResult.([]protocol.CallHierarchyOutgoingCall)
	if !ok {
		t.Fatalf("handleOutgoingCalls = %#v, want []protocol.CallHierarchyOutgoingCall", outResult)
	}
	golanceLocs := make([]navLoc, 0, len(calls))
	for i := range calls {
		golanceLocs = append(golanceLocs, locFromLSP(protocol.Location{URI: calls[i].To.URI, Range: calls[i].To.SelectionRange}))
	}
	goplsLocs := make([]navLoc, 0, len(goplsCallees))
	for _, e := range goplsCallees {
		goplsLocs = append(goplsLocs, e.otherEnd)
	}
	if !locsEqualSet(golanceLocs, goplsLocs) {
		t.Errorf("outgoing-call target mismatch at %s %q:\n golance = [%s]\n gopls   = [%s]",
			p.file, p.ident, locsString(golanceLocs), locsString(goplsLocs))
	}
}

// checkIncomingCalls is checkOutgoingCalls' incoming-call counterpart.
func checkIncomingCalls(t *testing.T, s *Server, p *navPos, item *protocol.CallHierarchyItem, goplsCallers []callHierarchyEdge) {
	t.Helper()
	inResult, err := s.handleIncomingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyIncomingCallsParams{Item: *item}))
	if err != nil {
		t.Fatalf("golance handleIncomingCalls: %v", err)
	}
	calls, ok := inResult.([]protocol.CallHierarchyIncomingCall)
	if !ok {
		t.Fatalf("handleIncomingCalls = %#v, want []protocol.CallHierarchyIncomingCall", inResult)
	}
	golanceLocs := make([]navLoc, 0, len(calls))
	for i := range calls {
		golanceLocs = append(golanceLocs, locFromLSP(protocol.Location{URI: calls[i].From.URI, Range: calls[i].From.SelectionRange}))
	}
	goplsLocs := make([]navLoc, 0, len(goplsCallers))
	for _, e := range goplsCallers {
		goplsLocs = append(goplsLocs, e.otherEnd)
	}
	if !locsEqualSet(golanceLocs, goplsLocs) {
		t.Errorf("incoming-call source mismatch at %s %q:\n golance = [%s]\n gopls   = [%s]",
			p.file, p.ident, locsString(golanceLocs), locsString(goplsLocs))
	}
}

// --- typeHierarchy/supertypes, typeHierarchy/subtypes (LSP-driven) ---

func TestNavAudit_TypeHierarchy(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)
	g := startGoplsLSP(t, t.TempDir(), root)
	for _, f := range navauditFiles(t, root) {
		g.didOpen(t, f)
	}

	cases := []struct {
		label string
		pos   navPos
		super bool // true: compare supertypes; false: compare subtypes
	}{
		{"Greeter supertypes (embeds Speaker)", navPos{"", "iface/iface.go", "Greeter", 1}, true},
		{"Speaker subtypes", navPos{"", "iface/iface.go", "Speaker", 1}, false},
		{"Named supertypes (embeds Base)", navPos{"", "iface/iface.go", "Named", 1}, true},
	}

	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			file, pos := navPosition(t, root, c.pos)

			prepResult, err := s.handlePrepareTypeHierarchy(context.Background(), mustMarshal(t, &protocol.TypeHierarchyPrepareParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
					Position:     pos,
				},
			}))
			if err != nil {
				t.Fatalf("golance handlePrepareTypeHierarchy: %v", err)
			}
			golanceItems, ok := prepResult.([]protocol.TypeHierarchyItem)
			if !ok || len(golanceItems) != 1 {
				t.Fatalf("handlePrepareTypeHierarchy = %#v, want exactly one item", prepResult)
			}

			goplsItems := g.prepareTypeHierarchy(t, file, pos)
			if len(goplsItems) != 1 {
				t.Fatalf("gopls prepareTypeHierarchy = %#v, want exactly one item", goplsItems)
			}

			golanceRelated := golanceRelatedTypeHierarchy(t, s, &golanceItems[0], c.super)
			goplsRelated := goplsRelatedTypeHierarchy(t, g, &goplsItems[0], c.super)

			golanceNames := typeHierarchyItemNames(golanceRelated)
			goplsNames := typeHierarchyItemNames(goplsRelated)
			if !slices.Equal(golanceNames, goplsNames) {
				t.Errorf("name-set mismatch: golance = %v, gopls = %v", golanceNames, goplsNames)
			}
		})
	}
}

// golanceRelatedTypeHierarchy calls handleTypeHierarchySupertypes or
// handleTypeHierarchySubtypes for item, depending on super.
func golanceRelatedTypeHierarchy(t *testing.T, s *Server, item *protocol.TypeHierarchyItem, super bool) []protocol.TypeHierarchyItem {
	t.Helper()
	if super {
		result, err := s.handleTypeHierarchySupertypes(context.Background(), mustMarshal(t, &protocol.TypeHierarchySupertypesParams{Item: *item}))
		if err != nil {
			t.Fatalf("golance handleTypeHierarchySupertypes: %v", err)
		}
		items, ok := result.([]protocol.TypeHierarchyItem)
		if !ok {
			t.Fatalf("handleTypeHierarchySupertypes = %#v, want []protocol.TypeHierarchyItem", result)
		}
		return items
	}
	result, err := s.handleTypeHierarchySubtypes(context.Background(), mustMarshal(t, &protocol.TypeHierarchySubtypesParams{Item: *item}))
	if err != nil {
		t.Fatalf("golance handleTypeHierarchySubtypes: %v", err)
	}
	items, ok := result.([]protocol.TypeHierarchyItem)
	if !ok {
		t.Fatalf("handleTypeHierarchySubtypes = %#v, want []protocol.TypeHierarchyItem", result)
	}
	return items
}

// goplsRelatedTypeHierarchy is golanceRelatedTypeHierarchy's gopls-side
// counterpart.
func goplsRelatedTypeHierarchy(t *testing.T, g *goplsLSP, item *protocol.TypeHierarchyItem, super bool) []protocol.TypeHierarchyItem {
	t.Helper()
	if super {
		return g.supertypes(t, item)
	}
	return g.subtypes(t, item)
}

// --- textDocument/rename, full end-to-end ---

// copyFixtureTree recursively copies src (a directory) into dst, both
// already-existing directories, for gopls rename -w to run against a
// disposable copy instead of the shared fixture.
func copyFixtureTree(t *testing.T, src, dst string) {
	t.Helper()
	// os.Root confines every path this walks and writes to src/dst
	// respectively -- including across the symlink resolution fs.WalkDir
	// performs on each entry -- closing the TOCTOU window a plain
	// filepath.WalkDir + os.ReadFile/os.WriteFile pair leaves open between
	// checking a path and later operating on it.
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", src, err)
	}
	defer func() {
		if err := srcRoot.Close(); err != nil {
			t.Errorf("close %s: %v", src, err)
		}
	}()
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dst, err)
	}
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dst, err)
	}
	defer func() {
		if err := dstRoot.Close(); err != nil {
			t.Errorf("close %s: %v", dst, err)
		}
	}()

	err = fs.WalkDir(srcRoot.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return dstRoot.MkdirAll(rel, 0o750)
		}
		data, err := srcRoot.ReadFile(rel)
		if err != nil {
			return err
		}
		f, err := dstRoot.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		return cerr
	})
	if err != nil {
		t.Fatalf("copyFixtureTree(%s, %s): %v", src, dst, err)
	}
}

func TestNavAudit_Rename(t *testing.T) {
	requireGopls(t)
	s, _, root := newTestServer(t)

	cases := []struct {
		pos     navPos
		newName string
	}{
		{navPos{"generic struct, cross-package fanout", "generics/generics.go", "Box", 1}, "RenamedBox"},
		{navPos{"func referenced from both test-file kinds", "testpkg/testpkg.go", "Symbol", 1}, "Answer"},
	}

	for _, c := range cases {
		t.Run(c.pos.label, func(t *testing.T) {
			file, pos := navPosition(t, root, c.pos)
			golanceByRel := golanceRenameEdits(t, s, root, file, pos, c.newName)
			goplsByRel := goplsRenameEdits(t, root, &c.pos, pos, c.newName)
			compareRenameResults(t, &c.pos, golanceByRel, goplsByRel)
		})
	}
}

// golanceRenameEdits calls handleRename at (file, pos) and applies its
// WorkspaceEdit to each touched file's original content, keyed by path
// relative to root.
func golanceRenameEdits(t *testing.T, s *Server, root, file string, pos protocol.Position, newName string) map[string]string {
	t.Helper()
	result, err := s.handleRename(context.Background(), mustMarshal(t, &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
		NewName: newName,
	}))
	if err != nil {
		t.Fatalf("golance handleRename: %v", err)
	}
	edit, ok := result.(*protocol.WorkspaceEdit)
	if !ok || edit == nil {
		t.Fatalf("handleRename = %#v, want *protocol.WorkspaceEdit", result)
	}
	byRel := map[string]string{}
	for u, edits := range edit.Changes {
		f := u.FsPath()
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatalf("filepath.Rel: %v", err)
		}
		byRel[rel] = applyTextEdits(t, mustReadFile(t, f), edits)
	}
	return byRel
}

// goplsRenameEdits runs `gopls rename -w -l` against a disposable copy of
// root and reads back every file it reports touching, keyed by path
// relative to that copy (directly comparable to golanceRenameEdits' keys,
// both relative to their own root).
func goplsRenameEdits(t *testing.T, root string, p *navPos, pos protocol.Position, newName string) map[string]string {
	t.Helper()
	// Resolved (not t.TempDir()'s raw path) so it matches the absolute
	// paths gopls itself emits: on macOS, t.TempDir() lives under a
	// /var/folders symlink target of /private/var/folders, and gopls's own
	// output resolves that symlink, making a filepath.Rel against the
	// unresolved path spuriously wander through a chain of ".." segments
	// instead of landing inside tmpRoot (see the mission's own
	// /tmp-vs-/private/tmp note).
	tmpRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	copyFixtureTree(t, root, tmpRoot)
	tmpFile := filepath.Join(tmpRoot, "navaudit", filepath.FromSlash(p.file))
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
		byRel[rel] = string(data)
	}
	return byRel
}

// compareRenameResults reports (via reportMismatchf, see its own doc) any
// difference between golance's and gopls's renamed file sets/contents for
// p.
func compareRenameResults(t *testing.T, p *navPos, golanceByRel, goplsByRel map[string]string) {
	t.Helper()
	if len(golanceByRel) != len(goplsByRel) {
		reportMismatchf(t, "Rename", p.label, "changed-file-set mismatch: golance touched %v, gopls touched %v", relKeys(golanceByRel), relKeys(goplsByRel))
		return
	}
	for rel, golanceContent := range golanceByRel {
		goplsContent, ok := goplsByRel[rel]
		if !ok {
			reportMismatchf(t, "Rename", p.label, "golance touched %s, gopls did not", rel)
			continue
		}
		if golanceContent != goplsContent {
			reportMismatchf(t, "Rename", p.label, "content mismatch in %s (showing byte lengths only): golance %d bytes, gopls %d bytes", rel, len(golanceContent), len(goplsContent))
		}
	}
}

func splitNonEmptyLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			line := s[start:i]
			if line != "" && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}

func relKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
