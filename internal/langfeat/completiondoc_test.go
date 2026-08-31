package langfeat_test

import (
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestResolveCompletionDoc_SamePackageSelector(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completiondoc", "completiondoc.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return w.Size") + len("return w.")

	got, err := langfeat.ResolveCompletionDoc(cp, reader, langfeat.CompletionDocKey{File: path, Offset: offset, Label: "Size"})
	if err != nil {
		t.Fatalf("ResolveCompletionDoc: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveCompletionDoc returned nil, want a result")
	}
	if !strings.Contains(got.Doc, "Size is the widget's size.") {
		t.Errorf("Doc = %q, want Size's doc comment", got.Doc)
	}
}

func TestResolveCompletionDoc_SamePackageLexical(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completiondoc", "completiondoc.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return Helper()") + len("return ")

	got, err := langfeat.ResolveCompletionDoc(cp, reader, langfeat.CompletionDocKey{File: path, Offset: offset, Label: "Helper"})
	if err != nil {
		t.Fatalf("ResolveCompletionDoc: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveCompletionDoc returned nil, want a result")
	}
	if !strings.Contains(got.Doc, "Helper is a documented package-level function") {
		t.Errorf("Doc = %q, want Helper's doc comment", got.Doc)
	}
}

func TestResolveCompletionDoc_CrossPackage(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completiondoc", "completiondoc.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "var r typedefdep.Remote") + len("var r typedefdep.")

	got, err := langfeat.ResolveCompletionDoc(cp, reader, langfeat.CompletionDocKey{File: path, Offset: offset, Label: "Remote"})
	if err != nil {
		t.Fatalf("ResolveCompletionDoc: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveCompletionDoc returned nil, want a result")
	}
	if got.Doc != "" {
		t.Errorf("Doc = %q, want \"\" (cross-package: resolved via PkgPath/ObjPath instead)", got.Doc)
	}
	if got.PkgPath != "example.com/langfeatmod/typedefdep" {
		t.Errorf("PkgPath = %q, want typedefdep's import path", got.PkgPath)
	}
	if got.ObjPath == "" {
		t.Error("ObjPath is empty, want a resolvable objectpath")
	}
}

func TestResolveCompletionDoc_NoMatch(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completiondoc", "completiondoc.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return Helper()") + len("return ")

	got, err := langfeat.ResolveCompletionDoc(cp, reader, langfeat.CompletionDocKey{File: path, Offset: offset, Label: "NoSuchCandidate"})
	if err != nil {
		t.Fatalf("ResolveCompletionDoc: %v", err)
	}
	if got != nil {
		t.Errorf("ResolveCompletionDoc = %+v, want nil (no matching candidate)", got)
	}
}
