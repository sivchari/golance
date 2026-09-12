package server

import (
	"go/format"
	"testing"

	"go.lsp.dev/protocol"
)

// TestRangeFormatEdits_GoplsParity_DoesNotLeakIntoUnrelatedHunk verifies
// textDocument/rangeFormatting confines its edits to hunks that overlap the
// requested range: text has two independently misformatted lines (in funcA
// and funcC), separated by an already-formatted funcB. Requesting a range
// over only funcA's misformatted line must return an edit for funcA alone —
// gopls never widens a range-confined edit past a second, unrelated hunk
// (golang.org/x/tools/gopls/internal/golang/format.go computes edits per
// changed hunk and returns only the ones overlapping the requested range).
func TestRangeFormatEdits_GoplsParity_DoesNotLeakIntoUnrelatedHunk(t *testing.T) {
	text := []byte(`package p

func A() int {
	x  :=  1
	return x
}

func B() int {
	y := 2
	return y
}

func C() int {
	z  :=  3
	return z
}
`)
	formatted, err := format.Source(text)
	if err != nil {
		t.Fatalf("format.Source: %v", err)
	}

	// Covers only funcA's misformatted line ("x  :=  1", line index 3).
	rng := protocol.Range{
		Start: protocol.Position{Line: 3, Character: 0},
		End:   protocol.Position{Line: 4, Character: 0},
	}
	edits := rangeFormatEdits(text, formatted, rng)
	if len(edits) != 1 {
		t.Fatalf("rangeFormatEdits(rng over funcA) = %+v, want exactly 1 edit", edits)
	}
	edit := edits[0]

	// funcC's own misformatted line is line index 13 ("z  :=  3"); a
	// range-confined edit for funcA alone must end at or before it.
	const wantMaxEndLine = 4
	if edit.Range.End.Line > wantMaxEndLine {
		t.Fatalf("edit leaked past funcA into funcC: Range.End.Line = %d, want <= %d — edit: %+v", edit.Range.End.Line, wantMaxEndLine, edit)
	}
	if want := "\tx := 1\n"; edit.NewText != want {
		t.Errorf("edit.NewText = %q, want %q", edit.NewText, want)
	}
}

// TestRangeFormatEdits_GoplsParity_UnrelatedRegionUntouched verifies the
// unrelated, independently-misformatted funcC region is not present in the
// edits returned for a request confined to funcA: no returned edit's range
// reaches funcC's line, and the funcA edit's NewText is exactly what gofmt
// produces for that line in place, not a copy of some other region.
func TestRangeFormatEdits_GoplsParity_UnrelatedRegionUntouched(t *testing.T) {
	text := []byte(`package p

func A() int {
	x  :=  1
	return x
}

func B() int {
	y := 2
	return y
}

func C() int {
	z  :=  3
	return z
}
`)
	formatted, err := format.Source(text)
	if err != nil {
		t.Fatalf("format.Source: %v", err)
	}

	rng := protocol.Range{
		Start: protocol.Position{Line: 3, Character: 0},
		End:   protocol.Position{Line: 4, Character: 0},
	}
	edits := rangeFormatEdits(text, formatted, rng)
	const funcCLine = 13
	for _, e := range edits {
		if e.Range.Start.Line <= funcCLine && e.Range.End.Line >= funcCLine {
			t.Fatalf("edit %+v touches funcC's line %d, which rng never requested", e, funcCLine)
		}
	}

	// Requesting a range over funcC's own misformatted line must format
	// exactly that region, matching what gofmt produces for it in place.
	cRng := protocol.Range{
		Start: protocol.Position{Line: 13, Character: 0},
		End:   protocol.Position{Line: 14, Character: 0},
	}
	cEdits := rangeFormatEdits(text, formatted, cRng)
	if len(cEdits) != 1 {
		t.Fatalf("rangeFormatEdits(rng over funcC) = %+v, want exactly 1 edit", cEdits)
	}
	if want := "\tz := 3\n"; cEdits[0].NewText != want {
		t.Errorf("funcC edit.NewText = %q, want %q", cEdits[0].NewText, want)
	}
}
