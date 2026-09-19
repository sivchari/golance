package server

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/xref"
)

// referencesLocsFor returns the raw resolver.References locations for
// (file, pos) -- the same input handleIncomingCalls itself passes to
// foldIncomingCalls -- for tests that need to exercise foldIncomingCalls
// directly rather than through the full handler.
func referencesLocsFor(t *testing.T, s *Server, file string, pos protocol.Position, includeDecl bool) []xref.Location {
	t.Helper()
	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("referencesLocsFor: no index loaded")
	}
	line, col, ok := s.xrefPosition(file, pos)
	if !ok {
		t.Fatalf("referencesLocsFor: xrefPosition(%s) not ok", file)
	}
	locs, err := idx.resolver.References(context.Background(), file, line, col, includeDecl)
	if err != nil {
		t.Fatalf("referencesLocsFor: resolver.References: %v", err)
	}
	return locs
}

// sequentialFoldIncomingCalls is foldIncomingCalls' pre-parallelization
// algorithm, kept here as this test's own oracle: it folds locs into one
// entry per enclosing function declaration strictly in input order, one
// file parsed at a time. foldIncomingCalls' file-partitioned concurrent
// version must produce byte-for-byte the same result regardless of which
// file's goroutine happens to finish first, since every location's own
// conversion (chSourceFileFor/xrefRangeToLSP/enclosingCallItem) is
// unchanged by that refactor -- only the iteration order is.
func sequentialFoldIncomingCalls(s *Server, ctx context.Context, locs []xref.Location) []protocol.CallHierarchyIncomingCall {
	files := make(map[string]*chSourceFile)
	calls := make(map[protocol.Location]*protocol.CallHierarchyIncomingCall)
	var order []protocol.Location

	for _, loc := range locs {
		if err := ctx.Err(); err != nil {
			break
		}
		sf := s.chSourceFileFor(loc.File, files)
		if sf == nil || sf.astFile == nil {
			continue
		}
		line := loc.Line
		if sf.dirtyLinesOK {
			if mapped, ok := dirtyLineMap(sf.saved, sf.dirty, line); ok {
				line = mapped
			}
		}
		fromRange, ok := xrefRangeToLSP(sf.text, line, loc.Col, loc.EndCol)
		if !ok {
			continue
		}
		offset, ok := byteOffsetForPosition(sf.text, fromRange.Start)
		if !ok {
			continue
		}
		item, ok := enclosingCallItem(sf.fset, sf.astFile, sf.text, loc.File, sf.pkgPath, offset)
		if !ok {
			continue
		}

		key := protocol.Location{URI: item.URI, Range: item.Range}
		call, exists := calls[key]
		if !exists {
			call = &protocol.CallHierarchyIncomingCall{From: item}
			calls[key] = call
			order = append(order, key)
		}
		call.FromRanges = append(call.FromRanges, fromRange)
	}

	sort.Slice(order, func(i, j int) bool { return compareLocation(order[i], order[j]) })
	out := make([]protocol.CallHierarchyIncomingCall, 0, len(order))
	for _, key := range order {
		out = append(out, *calls[key])
	}
	return out
}

// TestFoldIncomingCalls_MatchesSequentialOracle pins foldIncomingCalls'
// output against sequentialFoldIncomingCalls (its pre-parallelization
// algorithm) across Add's real references in the callh fixture -- which
// span multiple files (callh.go itself, callh_test.go, and callhuser/
// callhuser.go) and multiple enclosing functions -- and across repeated
// runs, so unordered per-file goroutine completion can never change the
// result.
func TestFoldIncomingCalls_MatchesSequentialOracle(t *testing.T) {
	s, _, root := newTestServer(t)
	file := callhFile(t, root)
	pos := callhPos(t, file, "Add", 1)
	locs := referencesLocsFor(t, s, file, pos, false)
	if len(locs) < 2 {
		t.Fatalf("references(Add) = %d locations, want at least 2 to exercise multiple call sites", len(locs))
	}

	want := sequentialFoldIncomingCalls(s, context.Background(), locs)
	if len(want) == 0 {
		t.Fatal("sequentialFoldIncomingCalls(Add) = no calls, want at least one enclosing caller")
	}
	for run := range 3 {
		got := s.foldIncomingCalls(context.Background(), locs)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: foldIncomingCalls = %+v, want %+v", run, got, want)
		}
	}
}
