package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestInlayHints_ShortVarDecl(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "inlay.go")

	hints, err := langfeat.InlayHints(cp, path)
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}
	if len(hints) != 2 {
		t.Fatalf("InlayHints = %+v, want 2 hints", hints)
	}
	if hints[0].Label != ": int" {
		t.Errorf("hints[0].Label = %q, want %q", hints[0].Label, ": int")
	}
	if hints[1].Label != ": string" {
		t.Errorf("hints[1].Label = %q, want %q", hints[1].Label, ": string")
	}
	if hints[0].Offset >= hints[1].Offset {
		t.Errorf("hints not in source order: %+v", hints)
	}
}
