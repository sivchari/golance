package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestPrepareRename_LocalVar(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "rename", "rename.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "msg := fmt")

	got, err := langfeat.PrepareRename(cp, path, offset)
	if err != nil {
		t.Fatalf("PrepareRename: %v", err)
	}
	if got == nil {
		t.Fatal("PrepareRename returned nil, want a range")
	}
	if got.StartOffset != offset {
		t.Errorf("StartOffset = %d, want %d", got.StartOffset, offset)
	}
}

func TestPrepareRename_PackageName(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "rename", "rename.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "fmt.Sprintf")

	got, err := langfeat.PrepareRename(cp, path, offset)
	if err != nil {
		t.Fatalf("PrepareRename: %v", err)
	}
	if got != nil {
		t.Errorf("PrepareRename = %+v, want nil (package names are not renameable)", got)
	}
}

func TestPrepareRename_Keyword(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "rename", "rename.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return msg")

	got, err := langfeat.PrepareRename(cp, path, offset)
	if err != nil {
		t.Fatalf("PrepareRename: %v", err)
	}
	if got != nil {
		t.Errorf("PrepareRename = %+v, want nil (cursor is on the \"return\" keyword)", got)
	}
}
