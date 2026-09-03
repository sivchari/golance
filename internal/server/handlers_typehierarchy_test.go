package server

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// typehFile returns the absolute path to internal/server/testdata/module's
// typeh package's typeh.go.
func typehFile(t *testing.T, snapRoot string) string {
	t.Helper()
	return filepath.Join(snapRoot, "typeh", "typeh.go")
}

// prepareTypeHierarchyItem calls handlePrepareTypeHierarchy at (file, pos)
// and returns its single resulting item, failing the test if it does not
// return exactly one.
func prepareTypeHierarchyItem(t *testing.T, s *Server, file string, pos protocol.Position) protocol.TypeHierarchyItem {
	t.Helper()
	result, err := s.handlePrepareTypeHierarchy(context.Background(), mustMarshal(t, &protocol.TypeHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handlePrepareTypeHierarchy: %v", err)
	}
	items, ok := result.([]protocol.TypeHierarchyItem)
	if !ok || len(items) != 1 {
		t.Fatalf("handlePrepareTypeHierarchy = %#v, want exactly one item", result)
	}
	return items[0]
}

// supertypesOf and subtypesOf take item by pointer, not value: like
// relatedTypeHierarchy in production code, this avoids copying the 128-byte
// protocol.TypeHierarchyItem (gocritic's hugeParam).
func supertypesOf(t *testing.T, s *Server, item *protocol.TypeHierarchyItem) []protocol.TypeHierarchyItem {
	t.Helper()
	result, err := s.handleTypeHierarchySupertypes(context.Background(), mustMarshal(t, &protocol.TypeHierarchySupertypesParams{Item: *item}))
	if err != nil {
		t.Fatalf("handleTypeHierarchySupertypes: %v", err)
	}
	items, ok := result.([]protocol.TypeHierarchyItem)
	if !ok {
		t.Fatalf("handleTypeHierarchySupertypes = %#v, want []protocol.TypeHierarchyItem", result)
	}
	return items
}

func subtypesOf(t *testing.T, s *Server, item *protocol.TypeHierarchyItem) []protocol.TypeHierarchyItem {
	t.Helper()
	result, err := s.handleTypeHierarchySubtypes(context.Background(), mustMarshal(t, &protocol.TypeHierarchySubtypesParams{Item: *item}))
	if err != nil {
		t.Fatalf("handleTypeHierarchySubtypes: %v", err)
	}
	items, ok := result.([]protocol.TypeHierarchyItem)
	if !ok {
		t.Fatalf("handleTypeHierarchySubtypes = %#v, want []protocol.TypeHierarchyItem", result)
	}
	return items
}

// itemNames extracts items' own Name fields, sorted. Indexed rather than
// ranged by value: protocol.TypeHierarchyItem is a 128-byte struct
// (gocritic's rangeValCopy).
func itemNames(items []protocol.TypeHierarchyItem) []string {
	names := make([]string, len(items))
	for i := range items {
		names[i] = items[i].Name
	}
	sort.Strings(names)
	return names
}

func assertItemNames(t *testing.T, got []protocol.TypeHierarchyItem, want ...string) {
	t.Helper()
	gotNames := itemNames(got)
	sort.Strings(want)
	if len(gotNames) != len(want) {
		t.Fatalf("got %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("got %v, want %v", gotNames, want)
		}
	}
}

func TestHandlePrepareTypeHierarchy(t *testing.T) {
	s, _, root := newTestServer(t)
	file := typehFile(t, root)

	t.Run("interface declaration", func(t *testing.T) {
		pos := identPositionIn(t, file, mustReadFile(t, file), "I", 1)
		item := prepareTypeHierarchyItem(t, s, file, pos)
		if item.Name != "I" {
			t.Errorf("Name = %q, want %q", item.Name, "I")
		}
		if item.Kind != protocol.SymbolKindInterface {
			t.Errorf("Kind = %v, want SymbolKindInterface", item.Kind)
		}
		if item.Range != item.SelectionRange {
			t.Errorf("Range = %+v, SelectionRange = %+v, want equal", item.Range, item.SelectionRange)
		}
		if item.URI.FsPath() != file {
			t.Errorf("URI = %s, want %s", item.URI.FsPath(), file)
		}
		if item.Detail == nil || *item.Detail != "example.com/servermod/typeh" {
			t.Errorf("Detail = %v, want typeh's own package path", item.Detail)
		}
	})

	t.Run("concrete type declaration", func(t *testing.T) {
		pos := identPositionIn(t, file, mustReadFile(t, file), "S", 1)
		item := prepareTypeHierarchyItem(t, s, file, pos)
		if item.Name != "S" {
			t.Errorf("Name = %q, want %q", item.Name, "S")
		}
		if item.Kind != protocol.SymbolKindClass {
			t.Errorf("Kind = %v, want SymbolKindClass", item.Kind)
		}
	})

	t.Run("method identifier returns no item", func(t *testing.T) {
		pos := identPositionIn(t, file, mustReadFile(t, file), "F", 1)
		result, err := s.handlePrepareTypeHierarchy(context.Background(), mustMarshal(t, &protocol.TypeHierarchyPrepareParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if err != nil {
			t.Fatalf("handlePrepareTypeHierarchy: %v", err)
		}
		items, _ := result.([]protocol.TypeHierarchyItem)
		if len(items) != 0 {
			t.Errorf("handlePrepareTypeHierarchy = %#v, want no items (F is a method, not a type name)", result)
		}
	})
}

// TestHandleTypeHierarchySubtypes exercises the same I/J/S plus BI/BJ/BS
// shape gopls's own type hierarchy marker test poses its queries against
// (see internal/xref/typehierarchy_test.go), through the full LSP handler
// chain: prepare, then subtypes.
func TestHandleTypeHierarchySubtypes(t *testing.T) {
	s, _, root := newTestServer(t)
	file := typehFile(t, root)

	tests := []struct {
		name string
		want []string
	}{
		{"S", nil},
		{"I", []string{"J", "S", "BI", "BJ", "BS"}},
		{"J", []string{"S", "BJ", "BS"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos := identPositionIn(t, file, mustReadFile(t, file), tt.name, 1)
			item := prepareTypeHierarchyItem(t, s, file, pos)
			got := subtypesOf(t, s, &item)
			assertItemNames(t, got, tt.want...)
		})
	}
}

// TestHandleTypeHierarchySupertypes exercises the identical fixture in the
// Supertypes direction.
func TestHandleTypeHierarchySupertypes(t *testing.T) {
	s, _, root := newTestServer(t)
	file := typehFile(t, root)

	tests := []struct {
		name string
		want []string
	}{
		{"S", []string{"I", "J", "BI", "BJ"}},
		{"I", []string{"BI"}},
		{"J", []string{"I", "BI", "BJ"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos := identPositionIn(t, file, mustReadFile(t, file), tt.name, 1)
			item := prepareTypeHierarchyItem(t, s, file, pos)
			got := supertypesOf(t, s, &item)
			assertItemNames(t, got, tt.want...)
		})
	}
}

// TestHandleTypeHierarchySubtypes_ItemKindAndLocation pins that a
// cross-package subtype result (BS, only findable through the workspace
// facts index) carries the right Kind and a resolvable Location, and that
// supertypes/subtypes items round-trip back into a further prepare-free
// query (item.Range.Start is exactly what relatedTypeHierarchy re-resolves
// through the index).
func TestHandleTypeHierarchySubtypes_ItemKindAndLocation(t *testing.T) {
	s, _, root := newTestServer(t)
	file := typehFile(t, root)
	pos := identPositionIn(t, file, mustReadFile(t, file), "I", 1)
	item := prepareTypeHierarchyItem(t, s, file, pos)
	got := subtypesOf(t, s, &item)

	var bs, j *protocol.TypeHierarchyItem
	for i := range got {
		switch got[i].Name {
		case "BS":
			bs = &got[i]
		case "J":
			j = &got[i]
		}
	}
	if bs == nil {
		t.Fatalf("subtypes(I) missing BS (cross-package, index-only): %+v", got)
	}
	if bs.Kind != protocol.SymbolKindClass {
		t.Errorf("BS.Kind = %v, want SymbolKindClass", bs.Kind)
	}
	if bs.Detail == nil || *bs.Detail != "example.com/servermod/typehdep" {
		t.Errorf("BS.Detail = %v, want typehdep's package path", bs.Detail)
	}
	wantFile := filepath.Join(root, "typehdep", "typehdep.go")
	if bs.URI.FsPath() != wantFile {
		t.Errorf("BS.URI = %s, want %s", bs.URI.FsPath(), wantFile)
	}

	if j == nil {
		t.Fatalf("subtypes(I) missing J (a same-package interface subtype): %+v", got)
	}
	if j.Kind != protocol.SymbolKindInterface {
		t.Errorf("J.Kind = %v, want SymbolKindInterface", j.Kind)
	}

	// A returned item's own Range.Start must resolve back through the index
	// on a further query, the same round trip a real client performs when
	// expanding a tree node.
	grandchildren := subtypesOf(t, s, j)
	assertItemNames(t, grandchildren, "S", "BJ", "BS")
}

// TestHandleTypeHierarchySupertypes_IndexUnavailable pins supertypes'
// index-required readiness contract: it must answer indexUnavailableError,
// not an empty result, while the workspace facts index has not finished
// building -- mirroring textDocument/implementation (see
// TestIndexUnavailable_ReturnsDistinctErrorNotEmptyResult) and
// callHierarchy/incomingCalls (TestHandleIncomingCalls_IndexUnavailable).
func TestHandleTypeHierarchySupertypes_IndexUnavailable(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/typeh"].GoFiles[0]
	pos := identPositionIn(t, file, mustReadFile(t, file), "S", 1)

	result, err := s.handleTypeHierarchySupertypes(context.Background(), mustMarshal(t, &protocol.TypeHierarchySupertypesParams{
		Item: protocol.TypeHierarchyItem{
			Name: "S",
			URI:  uri.File(file),
			Range: protocol.Range{
				Start: pos,
				End:   protocol.Position{Line: pos.Line, Character: pos.Character + 1},
			},
		},
	}))
	checkIndexUnavailableError(t, "supertypes", err)
	if result != nil {
		t.Errorf("supertypes: result = %#v, want nil", result)
	}
}

// TestHandleTypeHierarchySubtypes_IndexUnavailable is
// TestHandleTypeHierarchySupertypes_IndexUnavailable's Subtypes
// counterpart.
func TestHandleTypeHierarchySubtypes_IndexUnavailable(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/typeh"].GoFiles[0]
	pos := identPositionIn(t, file, mustReadFile(t, file), "I", 1)

	result, err := s.handleTypeHierarchySubtypes(context.Background(), mustMarshal(t, &protocol.TypeHierarchySubtypesParams{
		Item: protocol.TypeHierarchyItem{
			Name: "I",
			URI:  uri.File(file),
			Range: protocol.Range{
				Start: pos,
				End:   protocol.Position{Line: pos.Line, Character: pos.Character + 1},
			},
		},
	}))
	checkIndexUnavailableError(t, "subtypes", err)
	if result != nil {
		t.Errorf("subtypes: result = %#v, want nil", result)
	}
}

// TestHandlePrepareTypeHierarchy_WorksWithoutIndex pins prepare's looser
// readiness contract: it resolves entirely through checkedFile's on-demand
// type-check engine, never the workspace facts index, so it keeps
// answering while the index has not finished building -- unlike
// supertypes/subtypes (see TestHandleTypeHierarchySupertypes_IndexUnavailable).
func TestHandlePrepareTypeHierarchy_WorksWithoutIndex(t *testing.T) {
	s, snap := newTestServerNoIndex(t)
	file := snap.Packages["example.com/servermod/typeh"].GoFiles[0]
	pos := identPositionIn(t, file, mustReadFile(t, file), "I", 1)

	item := prepareTypeHierarchyItem(t, s, file, pos)
	if item.Name != "I" {
		t.Fatalf("prepareTypeHierarchy without an index: item.Name = %q, want %q", item.Name, "I")
	}
}
