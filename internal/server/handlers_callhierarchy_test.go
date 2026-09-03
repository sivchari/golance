package server

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// callhFile returns the absolute path to internal/server/testdata/module's
// callh package's callh.go.
func callhFile(t *testing.T, snapRoot string) string {
	t.Helper()
	return filepath.Join(snapRoot, "callh", "callh.go")
}

// callhPos returns the LSP position of the occurrence-th (1-based)
// identifier named ident in file.
func callhPos(t *testing.T, file, ident string, occurrence int) protocol.Position {
	t.Helper()
	return identPositionIn(t, file, mustReadFile(t, file), ident, occurrence)
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

// outgoingCallsFor prepares the call hierarchy item at funcName's own
// declaration in file (its first occurrence) and returns its outgoing
// calls.
func outgoingCallsFor(t *testing.T, s *Server, file, funcName string) []protocol.CallHierarchyOutgoingCall {
	t.Helper()
	pos := callhPos(t, file, funcName, 1)
	item := preparedItem(t, s, file, pos)

	result, err := s.handleOutgoingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyOutgoingCallsParams{Item: item}))
	if err != nil {
		t.Fatalf("handleOutgoingCalls: %v", err)
	}
	calls, ok := result.([]protocol.CallHierarchyOutgoingCall)
	if !ok {
		t.Fatalf("handleOutgoingCalls = %#v, want []protocol.CallHierarchyOutgoingCall", result)
	}
	return calls
}

// findOutgoingCall returns the entry of calls whose callee is named name.
func findOutgoingCall(calls []protocol.CallHierarchyOutgoingCall, name string) (protocol.CallHierarchyOutgoingCall, bool) {
	for i := range calls {
		if calls[i].To.Name == name {
			return calls[i], true
		}
	}
	return protocol.CallHierarchyOutgoingCall{}, false
}

// incomingCallsFor calls handleIncomingCalls for item and returns its
// result.
func incomingCallsFor(t *testing.T, s *Server, item *protocol.CallHierarchyItem) []protocol.CallHierarchyIncomingCall {
	t.Helper()
	result, err := s.handleIncomingCalls(context.Background(), mustMarshal(t, &protocol.CallHierarchyIncomingCallsParams{Item: *item}))
	if err != nil {
		t.Fatalf("handleIncomingCalls: %v", err)
	}
	calls, ok := result.([]protocol.CallHierarchyIncomingCall)
	if !ok {
		t.Fatalf("handleIncomingCalls = %#v, want []protocol.CallHierarchyIncomingCall", result)
	}
	return calls
}

// findIncomingCall returns the entry of calls whose caller is named name.
func findIncomingCall(calls []protocol.CallHierarchyIncomingCall, name string) (protocol.CallHierarchyIncomingCall, bool) {
	for i := range calls {
		if calls[i].From.Name == name {
			return calls[i], true
		}
	}
	return protocol.CallHierarchyIncomingCall{}, false
}

func TestHandlePrepareCallHierarchy(t *testing.T) {
	s, _, root := newTestServer(t)
	file := callhFile(t, root)

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
	file := callhFile(t, root)

	t.Run("Caller: direct calls plus an interface-mediated call", func(t *testing.T) {
		checkOutgoingCallsCaller(t, s, file)
	})

	t.Run("Describe: stdlib, a different workspace package, builtin filtered", func(t *testing.T) {
		checkOutgoingCallsDescribe(t, s, file)
	})
}

// checkOutgoingCallsCaller asserts Caller's outgoing calls are exactly Add
// (called twice), Describe, and Greet (through the Greeter interface).
func checkOutgoingCallsCaller(t *testing.T, s *Server, file string) {
	t.Helper()
	calls := outgoingCallsFor(t, s, file, "Caller")
	if len(calls) != 3 {
		t.Fatalf("handleOutgoingCalls returned %d entries, want 3 (Add, Describe, Greet): %+v", len(calls), calls)
	}
	for name, wantFromRanges := range map[string]int{"Add": 2, "Describe": 1, "Greet": 1} {
		c, ok := findOutgoingCall(calls, name)
		if !ok {
			t.Errorf("outgoing calls did not include %s: %+v", name, calls)
			continue
		}
		if len(c.FromRanges) != wantFromRanges {
			t.Errorf("%s fromRanges = %d, want %d", name, len(c.FromRanges), wantFromRanges)
		}
	}
}

// checkOutgoingCallsDescribe asserts Describe's outgoing calls include
// callhdep.Double (a different workspace package) and fmt.Sprintf (the
// standard library), and never the builtin len.
func checkOutgoingCallsDescribe(t *testing.T, s *Server, file string) {
	t.Helper()
	calls := outgoingCallsFor(t, s, file, "Describe")

	if _, ok := findOutgoingCall(calls, "len"); ok {
		t.Errorf("outgoing calls included the builtin len, want it filtered: %+v", calls)
	}

	double, ok := findOutgoingCall(calls, "Double")
	if !ok {
		t.Errorf("outgoing calls did not include callhdep.Double (a different workspace package): %+v", calls)
	} else if !contains(*double.To.Detail, "example.com/servermod/callhdep") {
		t.Errorf("Double's Detail = %q, want it to name callhdep's package path", *double.To.Detail)
	}

	sprintf, ok := findOutgoingCall(calls, "Sprintf")
	if !ok {
		t.Errorf("outgoing calls did not include fmt.Sprintf (the standard library): %+v", calls)
	} else if !contains(*sprintf.To.Detail, "fmt") {
		t.Errorf("Sprintf's Detail = %q, want it to name the fmt package", *sprintf.To.Detail)
	}
}

func TestHandleIncomingCalls(t *testing.T) {
	s, _, root := newTestServer(t)
	file := callhFile(t, root)

	t.Run("Add: same-caller aggregation, function literal, cross-package, and test file", func(t *testing.T) {
		checkIncomingCallsAdd(t, s, file, root)
	})

	t.Run("interface-mediated: Robot.Greet is reachable through Caller's g.Greet() call", func(t *testing.T) {
		checkIncomingCallsRobotGreet(t, s, file)
	})
}

// checkIncomingCallsAdd asserts Add's incoming calls cover: Caller (called
// twice, fromRanges aggregation), WithLiteral (called from a nested
// function literal), UseAdd (a different package, findable only via the
// index), and TestAddFromTest (an in-package _test.go file).
func checkIncomingCallsAdd(t *testing.T, s *Server, file, root string) {
	t.Helper()
	pos := callhPos(t, file, "Add", 1) // Add's own declaration
	item := preparedItem(t, s, file, pos)
	calls := incomingCallsFor(t, s, &item)

	if c, ok := findIncomingCall(calls, "Caller"); !ok {
		t.Error("incoming calls did not include Caller")
	} else if len(c.FromRanges) != 2 {
		t.Errorf("Caller's FromRanges = %d, want 2 (Add is called twice from Caller)", len(c.FromRanges))
	}

	if c, ok := findIncomingCall(calls, "WithLiteral"); !ok {
		t.Error("incoming calls did not include WithLiteral (called from a nested function literal)")
	} else if len(c.FromRanges) != 1 {
		t.Errorf("WithLiteral's FromRanges = %d, want 1", len(c.FromRanges))
	}

	if c, ok := findIncomingCall(calls, "UseAdd"); !ok {
		t.Error("incoming calls did not include UseAdd (a different package, findable only via the index)")
	} else if c.From.URI.FsPath() != filepath.Join(root, "callhuser", "callhuser.go") {
		t.Errorf("UseAdd's From.URI = %s, want callhuser.go", c.From.URI.FsPath())
	}

	if _, ok := findIncomingCall(calls, "TestAddFromTest"); !ok {
		t.Error("incoming calls did not include TestAddFromTest (an in-package _test.go file)")
	}
}

// checkIncomingCallsRobotGreet asserts Robot's own Greet method's incoming
// calls include Caller, which reaches it only through the Greeter
// interface.
func checkIncomingCallsRobotGreet(t *testing.T, s *Server, file string) {
	t.Helper()
	pos := callhPos(t, file, "Greet", 2) // Robot's own Greet method declaration
	item := preparedItem(t, s, file, pos)
	calls := incomingCallsFor(t, s, &item)

	if _, ok := findIncomingCall(calls, "Caller"); !ok {
		t.Errorf("incoming calls on Robot.Greet did not include Caller (calls it through the Greeter interface): %+v", calls)
	}
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
	file := callhFile(t, root)
	pos := callhPos(t, file, "Add", 1)
	item := preparedItem(t, s, file, pos)
	calls := incomingCallsFor(t, s, &item)

	locs := make([]protocol.Location, len(calls))
	for i := range calls {
		locs[i] = protocol.Location{URI: calls[i].From.URI, Range: calls[i].From.Range}
	}
	if !sort.SliceIsSorted(locs, func(i, j int) bool { return compareLocation(locs[i], locs[j]) }) {
		t.Errorf("handleIncomingCalls result is not sorted by (URI, Range.Start): %+v", locs)
	}
}
