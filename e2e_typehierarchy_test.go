package golance_test

import (
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eTypeHierarchyLocs records the exact 0-based positions
// TestE2E_TypeHierarchy queries, captured while the synthetic module is
// written so nothing is re-parsed at query time.
type e2eTypeHierarchyLocs struct {
	file string // main.go

	iPos protocol.Position // "I" at its own declaration
}

func writeE2ETypeHierarchyModule(t *testing.T) (string, e2eTypeHierarchyLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eTypeHierarchyLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2etypehier\n\ngo 1.23\n")

	const src = `package main

// I declares F.
type I interface {
	F()
}

// S implements I.
type S int

func (S) F() {}

func main() {
	var i I = S(0)
	_ = i
}
`
	locs.file = writeE2EFile(t, root, "main.go", src)
	locs.iPos = mustPos(t, src, "type I interface {", "I")

	return root, locs
}

// TestE2E_TypeHierarchy drives a real golance binary over stdio, exercising
// the full prepareTypeHierarchy -> subtypes -> supertypes round trip:
// prepare on I's own declaration, subtypes on that item finds S, and
// supertypes on THAT result item (S) finds its way back to I -- the same
// round trip an editor performs when a user expands a type hierarchy tree
// node.
func TestE2E_TypeHierarchy(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2ETypeHierarchyModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.file)
	c.waitForIndexReady(t)

	prepResp := c.call(t, protocol.MethodTextDocumentPrepareTypeHierarchy, &protocol.TypeHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.file)},
			Position:     locs.iPos,
		},
	}, e2eRequestBudget)
	if len(prepResp.Error) > 0 {
		t.Fatalf("prepareTypeHierarchy failed: %s", prepResp.Error)
	}
	var prepItems []protocol.TypeHierarchyItem
	if err := protocol.Unmarshal(prepResp.Result, &prepItems); err != nil {
		t.Fatalf("unmarshal prepareTypeHierarchy result: %v", err)
	}
	if len(prepItems) != 1 || prepItems[0].Name != "I" {
		t.Fatalf("prepareTypeHierarchy = %+v, want a single I item", prepItems)
	}
	if prepItems[0].Kind != protocol.SymbolKindInterface {
		t.Fatalf("prepareTypeHierarchy I.Kind = %v, want SymbolKindInterface", prepItems[0].Kind)
	}
	iItem := prepItems[0]

	subItems := waitForNonEmptyTypeHierarchySubtypes(t, c, &iItem, e2eIndexBudget)
	if len(subItems) != 1 || subItems[0].Name != "S" {
		t.Fatalf("subtypes(I) = %+v, want a single S entry", subItems)
	}
	if subItems[0].Kind != protocol.SymbolKindClass {
		t.Fatalf("subtypes(I) S.Kind = %v, want SymbolKindClass", subItems[0].Kind)
	}
	sItem := subItems[0]

	supResp := c.callRetryIndexUnavailable(t, protocol.MethodTypeHierarchySupertypes, &protocol.TypeHierarchySupertypesParams{Item: sItem}, e2eIndexBudget)
	if len(supResp.Error) > 0 {
		t.Fatalf("supertypes failed: %s", supResp.Error)
	}
	var supItems []protocol.TypeHierarchyItem
	if err := protocol.Unmarshal(supResp.Result, &supItems); err != nil {
		t.Fatalf("unmarshal supertypes result: %v", err)
	}
	if len(supItems) != 1 || supItems[0].Name != "I" {
		t.Fatalf("supertypes(S) = %+v, want a single I entry", supItems)
	}
}

// waitForNonEmptyTypeHierarchySubtypes polls typeHierarchy/subtypes for
// item until it returns at least one entry, or timeout elapses, riding out
// both the transient index-unavailable error (callRetryIndexUnavailable)
// and an ordinary empty result while the index build is still catching up
// -- the same pattern waitForNonEmptyIncomingCalls uses for
// callHierarchy/incomingCalls.
func waitForNonEmptyTypeHierarchySubtypes(t *testing.T, c *lspClient, item *protocol.TypeHierarchyItem, timeout time.Duration) []protocol.TypeHierarchyItem {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp := c.callRetryIndexUnavailable(t, protocol.MethodTypeHierarchySubtypes, &protocol.TypeHierarchySubtypesParams{Item: *item}, timeout)
		if len(resp.Error) > 0 {
			t.Fatalf("subtypes failed: %s", resp.Error)
		}
		var got []protocol.TypeHierarchyItem
		if err := protocol.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal subtypes result: %v", err)
		}
		if len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("subtypes returned no results within %s of the index becoming ready", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
