package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestImportLinks(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "documentlink", "documentlink.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	got, err := langfeat.ImportLinks(cp, path)
	if err != nil {
		t.Fatalf("ImportLinks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ImportLinks returned %d links, want 2: %+v", len(got), got)
	}

	byPath := make(map[string]langfeat.ImportLink, len(got))
	for _, l := range got {
		byPath[l.PkgPath] = l
	}
	if _, ok := byPath["fmt"]; !ok {
		t.Errorf("ImportLinks = %+v, want an entry for \"fmt\"", got)
	}
	remote, ok := byPath["example.com/langfeatmod/typedefdep"]
	if !ok {
		t.Fatalf("ImportLinks = %+v, want an entry for the typedefdep import", got)
	}
	wantOffset := mustIndex(t, text, `"example.com/langfeatmod/typedefdep"`)
	if remote.Range.StartOffset != wantOffset {
		t.Errorf("typedefdep link StartOffset = %d, want %d", remote.Range.StartOffset, wantOffset)
	}
}
