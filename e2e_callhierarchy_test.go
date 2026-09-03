package golance_test

import (
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eCallHierarchyLocs records the exact 0-based positions
// TestE2E_CallHierarchy queries, captured while the synthetic module is
// written so nothing is re-parsed at query time.
type e2eCallHierarchyLocs struct {
	file string // main.go

	callerPos protocol.Position // "Caller" at its own declaration
}

func writeE2ECallHierarchyModule(t *testing.T) (string, e2eCallHierarchyLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eCallHierarchyLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2echain\n\ngo 1.23\n")

	const src = `package main

import "fmt"

// Greet returns a greeting for name.
func Greet(name string) string {
	return fmt.Sprintf("hello, %s", name)
}

// Caller calls Greet.
func Caller() string {
	return Greet("world")
}

func main() {
	fmt.Println(Caller())
}
`
	locs.file = writeE2EFile(t, root, "main.go", src)
	locs.callerPos = mustPos(t, src, "func Caller() string {", "Caller")

	return root, locs
}

// TestE2E_CallHierarchy drives a real golance binary over stdio, exercising
// the full prepareCallHierarchy -> outgoingCalls -> incomingCalls round
// trip: prepare on Caller's own declaration, outgoingCalls on that item
// finds Caller's one call to Greet, and incomingCalls on THAT result item
// (Greet) finds its way back to Caller -- the same round trip an editor
// performs when a user expands a call hierarchy tree node.
func TestE2E_CallHierarchy(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2ECallHierarchyModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.file)
	c.waitForIndexReady(t)

	prepResp := c.call(t, protocol.MethodTextDocumentPrepareCallHierarchy, &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.file)},
			Position:     locs.callerPos,
		},
	}, e2eRequestBudget)
	if len(prepResp.Error) > 0 {
		t.Fatalf("prepareCallHierarchy failed: %s", prepResp.Error)
	}
	var prepItems []protocol.CallHierarchyItem
	if err := protocol.Unmarshal(prepResp.Result, &prepItems); err != nil {
		t.Fatalf("unmarshal prepareCallHierarchy result: %v", err)
	}
	if len(prepItems) != 1 || prepItems[0].Name != "Caller" {
		t.Fatalf("prepareCallHierarchy = %+v, want a single Caller item", prepItems)
	}
	callerItem := prepItems[0]

	outResp := c.call(t, protocol.MethodCallHierarchyOutgoingCalls, &protocol.CallHierarchyOutgoingCallsParams{Item: callerItem}, e2eRequestBudget)
	if len(outResp.Error) > 0 {
		t.Fatalf("outgoingCalls failed: %s", outResp.Error)
	}
	var outCalls []protocol.CallHierarchyOutgoingCall
	if err := protocol.Unmarshal(outResp.Result, &outCalls); err != nil {
		t.Fatalf("unmarshal outgoingCalls result: %v", err)
	}
	if len(outCalls) != 1 || outCalls[0].To.Name != "Greet" {
		t.Fatalf("outgoingCalls(Caller) = %+v, want a single Greet entry", outCalls)
	}
	if len(outCalls[0].FromRanges) != 1 {
		t.Fatalf("outgoingCalls(Caller) Greet.FromRanges = %+v, want exactly one call site", outCalls[0].FromRanges)
	}
	greetItem := outCalls[0].To

	inCalls := waitForNonEmptyIncomingCalls(t, c, greetItem, e2eIndexBudget)
	if len(inCalls) != 1 || inCalls[0].From.Name != "Caller" {
		t.Fatalf("incomingCalls(Greet) = %+v, want a single Caller entry", inCalls)
	}
	if len(inCalls[0].FromRanges) != 1 {
		t.Fatalf("incomingCalls(Greet) Caller.FromRanges = %+v, want exactly one call site", inCalls[0].FromRanges)
	}
}

// waitForNonEmptyIncomingCalls polls callHierarchy/incomingCalls for item
// until it returns at least one entry, or timeout elapses, riding out both
// the transient index-unavailable error (callRetryIndexUnavailable) and an
// ordinary empty result -- the same short window waitForNonEmptyLocations
// rides out for a definition/references-shaped request.
func waitForNonEmptyIncomingCalls(t *testing.T, c *lspClient, item protocol.CallHierarchyItem, timeout time.Duration) []protocol.CallHierarchyIncomingCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp := c.callRetryIndexUnavailable(t, protocol.MethodCallHierarchyIncomingCalls, &protocol.CallHierarchyIncomingCallsParams{Item: item}, timeout)
		if len(resp.Error) > 0 {
			t.Fatalf("incomingCalls failed: %s", resp.Error)
		}
		var got []protocol.CallHierarchyIncomingCall
		if err := protocol.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal incomingCalls result: %v", err)
		}
		if len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("incomingCalls returned no results within %s of the index becoming ready", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
