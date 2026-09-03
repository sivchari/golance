package server

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// callhFile returns the absolute path to internal/server/testdata/module's
// callh package file name.
func callhFile(t *testing.T, snapRoot, name string) string {
	t.Helper()
	return filepath.Join(snapRoot, "callh", name)
}

// callhPos returns the LSP position of the occurrence-th (1-based)
// identifier named ident in file.
func callhPos(t *testing.T, file, ident string, occurrence int) protocol.Position {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return identPositionIn(t, file, data, ident, occurrence)
}

// preparedItem calls handlePrepareCallHierarchy at (file, pos) and returns
// its single resulting item, failing the test if it does not return exactly
// one.
func preparedItem(t *testing.T, s *Server, file string, pos protocol.Position) protocol.CallHierarchyItem {
	t.Helper()
	result, err := s.handlePrepareCallHierarchy(context.Background(), mustMarshal(t, &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handlePrepareCallHierarchy: %v", err)
	}
	items, ok := result.([]protocol.CallHierarchyItem)
	if !ok || len(items) != 1 {
		t.Fatalf("handlePrepareCallHierarchy = %#v, want exactly one item", result)
	}
	return items[0]
}

func TestHandlePrepareCallHierarchy(t *testing.T) {
	s, _, root := newTestServer(t)
	file := callhFile(t, root, "callh.go")

	t.Run("call site resolves to the declaration", func(t *testing.T) {
		pos := callhPos(t, file, "Add", 2) // first Add(1, 2) call in Caller
		item := preparedItem(t, s, file, pos)
		if item.Name != "Add" {
			t.Errorf("Name = %q, want %q", item.Name, "Add")
		}
		wantPos := callhPos(t, file, "Add", 1) // Add's own declaration
		if item.Range.Start != wantPos {
			t.Errorf("Range.Start = %+v, want %+v (Add's declaration)", item.Range.Start, wantPos)
		}
		if item.Range != item.SelectionRange {
			t.Errorf("Range = %+v, SelectionRange = %+v, want equal", item.Range, item.SelectionRange)
		}
		if item.URI.FsPath() != file {
			t.Errorf("URI = %s, want %s", item.URI.FsPath(), file)
		}
	})

	t.Run("non-func identifier returns no item", func(t *testing.T) {
		pos := callhPos(t, file, "sum", 1)
		result, err := s.handlePrepareCallHierarchy(context.Background(), mustMarshal(t, &protocol.CallHierarchyPrepareParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if err != nil {
			t.Fatalf("handlePrepareCallHierarchy: %v", err)
		}
		items, _ := result.([]protocol.CallHierarchyItem)
		if len(items) != 0 {
			t.Errorf("handlePrepareCallHierarchy = %#v, want no items (sum is a var)", result)
		}
	})
}

func TestHandleOutgoingCalls(t *testing.T) {
	s, _, root := newTestServer(t)
	file := callhFile(t, root, "callh.go")

	t.Run("Caller: direct calls plus an interface-mediated call", func(t *testing.T) {
		pos := callhPos(t, file, "Caller", 1)
		item := preparedItem(t, s, file, pos)

		result, err := s.handleOutgoingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyOutgoingCallsParams{Item: item}))
		if err != nil {
			t.Fatalf("handleOutgoingCalls: %v", err)
		}
		calls, ok := result.([]protocol.CallHierarchyOutgoingCall)
		if !ok {
			t.Fatalf("handleOutgoingCalls = %#v, want []protocol.CallHierarchyOutgoingCall", result)
		}
		names := make(map[string]int)
		for _, c := range calls {
			names[c.To.Name] = len(c.FromRanges)
		}
		if names["Add"] != 2 {
			t.Errorf("Add fromRanges = %d, want 2 (Add is called twice)", names["Add"])
		}
		if names["Describe"] != 1 {
			t.Errorf("Describe fromRanges = %d, want 1", names["Describe"])
		}
		if names["Greet"] != 1 {
			t.Errorf("Greet fromRanges = %d, want 1 (g.Greet() through the Greeter interface)", names["Greet"])
		}
		if len(calls) != 3 {
			t.Errorf("handleOutgoingCalls returned %d entries, want 3 (Add, Describe, Greet): %+v", len(calls), calls)
		}
	})

	t.Run("Describe: stdlib, a different workspace package, builtin filtered", func(t *testing.T) {
		pos := callhPos(t, file, "Describe", 1)
		item := preparedItem(t, s, file, pos)

		result, err := s.handleOutgoingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyOutgoingCallsParams{Item: item}))
		if err != nil {
			t.Fatalf("handleOutgoingCalls: %v", err)
		}
		calls, ok := result.([]protocol.CallHierarchyOutgoingCall)
		if !ok {
			t.Fatalf("handleOutgoingCalls = %#v, want []protocol.CallHierarchyOutgoingCall", result)
		}
		var sawDouble bool
		for _, c := range calls {
			if c.To.Name == "len" {
				t.Errorf("outgoing calls included the builtin len, want it filtered: %+v", calls)
			}
			if c.To.Name == "Double" {
				sawDouble = true
				if !contains(*c.To.Detail, "example.com/servermod/callhdep") {
					t.Errorf("Double's Detail = %q, want it to name callhdep's package path", *c.To.Detail)
				}
			}
		}
		if !sawDouble {
			t.Errorf("outgoing calls did not include callhdep.Double (a different workspace package): %+v", calls)
		}
		var sawSprintf bool
		for _, c := range calls {
			if c.To.Name == "Sprintf" {
				sawSprintf = true
				if !contains(*c.To.Detail, "fmt") {
					t.Errorf("Sprintf's Detail = %q, want it to name the fmt package", *c.To.Detail)
				}
			}
		}
		if !sawSprintf {
			t.Errorf("outgoing calls did not include fmt.Sprintf (the standard library): %+v", calls)
		}
	})
}

func TestHandleIncomingCalls(t *testing.T) {
	s, _, root := newTestServer(t)
	file := callhFile(t, root, "callh.go")

	t.Run("Add: same-caller aggregation, function literal, cross-package, and test file", func(t *testing.T) {
		pos := callhPos(t, file, "Add", 1) // Add's own declaration
		item := preparedItem(t, s, file, pos)

		result, err := s.handleIncomingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyIncomingCallsParams{Item: item}))
		if err != nil {
			t.Fatalf("handleIncomingCalls: %v", err)
		}
		calls, ok := result.([]protocol.CallHierarchyIncomingCall)
		if !ok {
			t.Fatalf("handleIncomingCalls = %#v, want []protocol.CallHierarchyIncomingCall", result)
		}

		byName := make(map[string]protocol.CallHierarchyIncomingCall)
		for _, c := range calls {
			byName[c.From.Name] = c
		}

		if c, ok := byName["Caller"]; !ok {
			t.Error("incoming calls did not include Caller")
		} else if len(c.FromRanges) != 2 {
			t.Errorf("Caller's FromRanges = %d, want 2 (Add is called twice from Caller)", len(c.FromRanges))
		}

		if c, ok := byName["WithLiteral"]; !ok {
			t.Error("incoming calls did not include WithLiteral (called from a nested function literal)")
		} else if len(c.FromRanges) != 1 {
			t.Errorf("WithLiteral's FromRanges = %d, want 1", len(c.FromRanges))
		}

		if c, ok := byName["UseAdd"]; !ok {
			t.Error("incoming calls did not include UseAdd (a different package, findable only via the index)")
		} else if c.From.URI.FsPath() != filepath.Join(root, "callhuser", "callhuser.go") {
			t.Errorf("UseAdd's From.URI = %s, want callhuser.go", c.From.URI.FsPath())
		}

		if _, ok := byName["TestAddFromTest"]; !ok {
			t.Error("incoming calls did not include TestAddFromTest (an in-package _test.go file)")
		}
	})

	t.Run("interface-mediated: Robot.Greet is reachable through Caller's g.Greet() call", func(t *testing.T) {
		pos := callhPos(t, file, "Greet", 2) // Robot's own Greet method declaration
		item := preparedItem(t, s, file, pos)

		result, err := s.handleIncomingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyIncomingCallsParams{Item: item}))
		if err != nil {
			t.Fatalf("handleIncomingCalls: %v", err)
		}
		calls, ok := result.([]protocol.CallHierarchyIncomingCall)
		if !ok {
			t.Fatalf("handleIncomingCalls = %#v, want []protocol.CallHierarchyIncomingCall", result)
		}
		var sawCaller bool
		for _, c := range calls {
			if c.From.Name == "Caller" {
				sawCaller = true
			}
		}
		if !sawCaller {
			t.Errorf("incoming calls on Robot.Greet did not include Caller (calls it through the Greeter interface): %+v", calls)
		}
	})
}

// TestHandleIncomingCalls_IndexUnavailable pins incomingCalls' index-required
// readiness contract: it must answer indexUnavailableError, not an empty
// result, while the workspace facts index has not finished building --
// mirroring textDocument/references (see TestIndexUnavailable_ReturnsDistinctErrorNotEmptyResult).
func TestHandleIncomingCalls_IndexUnavailable(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/callh"].GoFiles[0]
	pos := identPositionIn(t, file, mustReadFile(t, file), "Add", 1)

	result, err := s.handleIncomingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyIncomingCallsParams{
		Item: protocol.CallHierarchyItem{
			Name: "Add",
			URI:  uri.File(file),
			Range: protocol.Range{
				Start: pos,
				End:   protocol.Position{Line: pos.Line, Character: pos.Character + 3},
			},
		},
	}))
	checkIndexUnavailableError(t, "incomingCalls", err)
	if result != nil {
		t.Errorf("incomingCalls: result = %#v, want nil", result)
	}
}

// TestHandlePrepareOutgoingCalls_WorkWithoutIndex pins prepare/outgoing's
// looser readiness contract: both resolve entirely through checkedFile's
// on-demand type-check engine, never the workspace facts index, so they
// keep answering while the index has not finished building -- unlike
// incomingCalls (see TestHandleIncomingCalls_IndexUnavailable).
func TestHandlePrepareOutgoingCalls_WorkWithoutIndex(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/callh"].GoFiles[0]
	pos := identPositionIn(t, file, mustReadFile(t, file), "Caller", 1)

	item := preparedItem(t, s, file, pos)
	if item.Name != "Caller" {
		t.Fatalf("prepareCallHierarchy without an index: item.Name = %q, want %q", item.Name, "Caller")
	}

	result, err := s.handleOutgoingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyOutgoingCallsParams{Item: item}))
	if err != nil {
		t.Fatalf("handleOutgoingCalls without an index: %v", err)
	}
	calls, ok := result.([]protocol.CallHierarchyOutgoingCall)
	if !ok || len(calls) == 0 {
		t.Fatalf("handleOutgoingCalls without an index = %#v, want a non-empty result", result)
	}
}

// TestFoldIncomingCalls_ResultOrder pins foldIncomingCalls' deterministic
// ordering contract directly (compareLocation, by URI then range start), so
// a client sees the same order across identical requests.
func TestFoldIncomingCalls_ResultOrder(t *testing.T) {
	s, _, root := newTestServer(t)
	file := callhFile(t, root, "callh.go")
	pos := callhPos(t, file, "Add", 1)
	item := preparedItem(t, s, file, pos)

	result, err := s.handleIncomingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyIncomingCallsParams{Item: item}))
	if err != nil {
		t.Fatalf("handleIncomingCalls: %v", err)
	}
	calls, ok := result.([]protocol.CallHierarchyIncomingCall)
	if !ok {
		t.Fatalf("handleIncomingCalls = %#v, want []protocol.CallHierarchyIncomingCall", result)
	}
	locs := make([]protocol.Location, len(calls))
	for i, c := range calls {
		locs[i] = protocol.Location{URI: c.From.URI, Range: c.From.Range}
	}
	if !sort.SliceIsSorted(locs, func(i, j int) bool { return compareLocation(locs[i], locs[j]) }) {
		t.Errorf("handleIncomingCalls result is not sorted by (URI, Range.Start): %+v", locs)
	}
}
