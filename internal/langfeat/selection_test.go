package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestSelectionRanges_InnermostFirst(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "selection", "selection.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "a + b")

	got, err := langfeat.SelectionRanges(cp, path, offset)
	if err != nil {
		t.Fatalf("SelectionRanges: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("SelectionRanges returned %d ranges, want at least 3 (ident, binary expr, return stmt, ...): %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.StartOffset > prev.StartOffset || prev.EndOffset > cur.EndOffset {
			t.Fatalf("SelectionRanges[%d] = %+v does not contain SelectionRanges[%d] = %+v", i, cur, i-1, prev)
		}
		if cur.StartOffset == prev.StartOffset && cur.EndOffset == prev.EndOffset {
			t.Fatalf("SelectionRanges[%d] and [%d] have identical ranges %+v, want strictly growing steps", i-1, i, cur)
		}
	}
	// The innermost range must be the identifier itself.
	first := got[0]
	if first.StartOffset != offset {
		t.Errorf("innermost range StartOffset = %d, want %d (the identifier at the query offset)", first.StartOffset, offset)
	}
}

func TestSelectionRanges_NoNode(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "selection", "selection.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := len(text)

	got, err := langfeat.SelectionRanges(cp, path, offset)
	if err != nil {
		t.Fatalf("SelectionRanges: %v", err)
	}
	if len(got) == 0 {
		t.Error("SelectionRanges returned no ranges even at end-of-file, want at least the enclosing file node")
	}
}
