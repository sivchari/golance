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

// TestResolveCompletionDoc_CrossPackage_GenericField verifies
// ResolveCompletionDoc's cross-package branch resolves a completion
// candidate reached through an INSTANTIATED generic dependency type
// (genericdep.Box[Concrete].Msg): selectorObjectForLabel's
// types.LookupFieldOrMethod produces the same synthetic, as-instantiated
// field Var TestDependencyDefinition_Generics's field case does, so this
// exercises depcheck.OriginObject's normalization on a second, independent
// call site.
func TestResolveCompletionDoc_CrossPackage_GenericField(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "genericuse", "genericuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "b.Msg") + len("b.")

	got, err := langfeat.ResolveCompletionDoc(cp, reader, langfeat.CompletionDocKey{File: path, Offset: offset, Label: "Msg"})
	if err != nil {
		t.Fatalf("ResolveCompletionDoc: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveCompletionDoc returned nil, want a result")
	}
	const wantPkgPath = "example.com/langfeatmod/genericdep"
	if got.PkgPath != wantPkgPath {
		t.Errorf("PkgPath = %q, want %q", got.PkgPath, wantPkgPath)
	}
	if got.ObjPath == "" {
		t.Error("ObjPath is empty, want a resolvable objectpath (Box.Msg reached through an instantiated generic type)")
	}
}

// TestResolveCompletionDoc_UnimportedSelector is a regression test for
// Finding L3's completiondoc.go half: a completionItem/resolve request for
// an unimported-package-member candidate (e.g. "fmt.Sprintf" with fmt not
// yet imported — see langfeat.Unimported) used to resolve nothing at all,
// since objectForLabel's ordinary selector resolution can never find fmt
// (it was never imported). ResolveCompletionDoc now reports
// UnimportedSelector for this shape instead of a bare nil, so the server
// layer (which has the graph access this package deliberately lacks) can
// redo the same package-name lookup and still show a doc.
func TestResolveCompletionDoc_UnimportedSelector(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "fmt.Sp") + len("fmt.Sp")

	got, err := langfeat.ResolveCompletionDoc(cp, reader, langfeat.CompletionDocKey{File: path, Offset: offset, Label: "Sprintf"})
	if err != nil {
		t.Fatalf("ResolveCompletionDoc: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveCompletionDoc returned nil, want a result carrying UnimportedSelector")
	}
	if got.UnimportedSelector != "fmt" {
		t.Errorf("UnimportedSelector = %q, want fmt", got.UnimportedSelector)
	}
	if got.Doc != "" || got.PkgPath != "" {
		t.Errorf("got = %+v, want only UnimportedSelector set (this package cannot resolve fmt itself)", got)
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
