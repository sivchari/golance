package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestDocumentHighlight_ReadAndWrite(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "highlight", "highlight.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return total") + len("return ")

	got, err := langfeat.DocumentHighlight(cp, path, offset)
	if err != nil {
		t.Fatalf("DocumentHighlight: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("DocumentHighlight returned %d occurrences, want 3: %+v", len(got), got)
	}

	declOffset := mustIndex(t, text, "total := 0")
	assignOffset := mustIndex(t, text, "total += v")
	returnOffset := mustIndex(t, text, "return total") + len("return ")

	wantKind := map[int]langfeat.HighlightKind{
		declOffset:   langfeat.HighlightWrite,
		assignOffset: langfeat.HighlightWrite,
		returnOffset: langfeat.HighlightRead,
	}
	for _, h := range got {
		want, ok := wantKind[h.Range.StartOffset]
		if !ok {
			t.Errorf("unexpected highlight at offset %d: %+v", h.Range.StartOffset, h)
			continue
		}
		if h.Kind != want {
			t.Errorf("highlight at offset %d: Kind = %v, want %v", h.Range.StartOffset, h.Kind, want)
		}
	}
}

func TestDocumentHighlight_NoIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "highlight", "highlight.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\n\n// Accumulate")

	got, err := langfeat.DocumentHighlight(cp, path, offset)
	if err != nil {
		t.Fatalf("DocumentHighlight: %v", err)
	}
	if got != nil {
		t.Errorf("DocumentHighlight = %+v, want nil (no identifier at offset)", got)
	}
}
