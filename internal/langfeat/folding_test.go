package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestFoldingRanges_NestedBlocks(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "folding", "folding.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	got, err := langfeat.FoldingRanges(cp, path)
	if err != nil {
		t.Fatalf("FoldingRanges: %v", err)
	}

	funcLbrace := mustIndex(t, text, "{\n\ttotal := 0")
	ifLbrace := mustIndex(t, text, "{\n\t\tfor _, item")
	forLbrace := mustIndex(t, text, "{\n\t\t\ttotal += len(item)")

	funcRange := findFoldingRangeStartingAt(t, got, funcLbrace)
	ifRange := findFoldingRangeStartingAt(t, got, ifLbrace)
	forRange := findFoldingRangeStartingAt(t, got, forLbrace)

	if funcRange.Range.StartOffset >= ifRange.Range.StartOffset || ifRange.Range.EndOffset >= funcRange.Range.EndOffset {
		t.Errorf("if-range %+v is not nested inside func-range %+v", ifRange, funcRange)
	}
	if ifRange.Range.StartOffset >= forRange.Range.StartOffset || forRange.Range.EndOffset >= ifRange.Range.EndOffset {
		t.Errorf("for-range %+v is not nested inside if-range %+v", forRange, ifRange)
	}
}

func TestFoldingRanges_ImportBlock(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "folding", "folding.go")

	got, err := langfeat.FoldingRanges(cp, path)
	if err != nil {
		t.Fatalf("FoldingRanges: %v", err)
	}
	found := false
	for _, fr := range got {
		if fr.Kind == langfeat.FoldImports {
			found = true
		}
	}
	if !found {
		t.Errorf("FoldingRanges = %+v, want an import-block folding range", got)
	}
}

func TestFoldingRanges_StructBody(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "folding", "folding.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	got, err := langfeat.FoldingRanges(cp, path)
	if err != nil {
		t.Fatalf("FoldingRanges: %v", err)
	}
	structLbrace := mustIndex(t, text, "{\n\tWidth  int")
	fr := findFoldingRangeStartingAt(t, got, structLbrace)
	if fr.Kind != langfeat.FoldRegion {
		t.Errorf("struct body folding kind = %v, want FoldRegion", fr.Kind)
	}
}

// findFoldingRangeStartingAt returns the FoldingRangeInfo in got whose
// Range.StartOffset equals offset, failing the test if none does.
func findFoldingRangeStartingAt(t *testing.T, got []langfeat.FoldingRangeInfo, offset int) langfeat.FoldingRangeInfo {
	t.Helper()
	for _, fr := range got {
		if fr.Range.StartOffset == offset {
			return fr
		}
	}
	t.Fatalf("no folding range in %+v starts at offset %d", got, offset)
	return langfeat.FoldingRangeInfo{}
}
