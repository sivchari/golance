package langfeat_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
)

// applyEdits applies edits (assumed non-overlapping) to src, in descending
// start-offset order so earlier offsets stay valid as later ones are
// applied.
func applyEdits(src string, edits []langfeat.Edit) string {
	sorted := append([]langfeat.Edit(nil), edits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Range.StartOffset > sorted[j].Range.StartOffset })
	b := []byte(src)
	for _, e := range sorted {
		tail := append([]byte(e.NewText), b[e.Range.EndOffset:]...)
		b = append(b[:e.Range.StartOffset], tail...)
	}
	return string(b)
}

func TestOrganizeImportsAction_AddsMissing(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F() {\n\tfmt.Println(\"hi\")\n}\n"
	action, ok, err := langfeat.OrganizeImportsAction(file, []byte(src))
	if err != nil {
		t.Fatalf("OrganizeImportsAction: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if action.Kind != langfeat.ActionSourceOrganizeImports {
		t.Errorf("Kind = %v, want ActionSourceOrganizeImports", action.Kind)
	}
	if len(action.Edits) != 1 {
		t.Fatalf("len(Edits) = %d, want 1", len(action.Edits))
	}
	if !strings.Contains(action.Edits[0].NewText, `"fmt"`) {
		t.Errorf("NewText = %q, want it to add the fmt import", action.Edits[0].NewText)
	}
}

func TestOrganizeImportsAction_NoChange(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n"
	_, ok, err := langfeat.OrganizeImportsAction(file, []byte(src))
	if err != nil {
		t.Fatalf("OrganizeImportsAction: %v", err)
	}
	if ok {
		t.Error("ok = true for already-organized source, want false")
	}
}

// TestOrganizeImportsAction_MinimalEdit checks that the edit is confined to
// the changed span rather than replacing the whole file, and that applying
// it reproduces the same result OrganizeImports itself would produce.
func TestOrganizeImportsAction_MinimalEdit(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "no imports to import block insertion",
			src:  "package foo\n\nfunc F() {\n\tfmt.Println(\"hi\")\n}\n",
		},
		{
			name: "unsorted imports",
			src:  "package foo\n\nimport (\n\t\"strconv\"\n\t\"fmt\"\n)\n\nfunc F() string {\n\treturn fmt.Sprintf(\"%s\", strconv.Itoa(1))\n}\n",
		},
		{
			name: "unused import removal",
			src:  "package foo\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc F() {\n\tfmt.Println(\"hi\")\n}\n",
		},
		{
			name: "no trailing newline (edit reaches EOF)",
			src:  "package foo\n\nfunc F() {\n\tfmt.Println(\"hi\")\n}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := t.TempDir() + "/foo.go"
			text := []byte(tt.src)
			want, err := langfeat.OrganizeImports(file, text)
			if err != nil {
				t.Fatalf("OrganizeImports: %v", err)
			}

			action, ok, err := langfeat.OrganizeImportsAction(file, text)
			if err != nil {
				t.Fatalf("OrganizeImportsAction: %v", err)
			}
			if !ok {
				t.Fatal("ok = false, want true")
			}
			if len(action.Edits) != 1 {
				t.Fatalf("len(Edits) = %d, want 1", len(action.Edits))
			}
			edit := action.Edits[0]

			if got := applyEdits(tt.src, action.Edits); got != string(want) {
				t.Errorf("applying the edit = %q, want %q", got, string(want))
			}
			if edit.Range.StartOffset == 0 && edit.Range.EndOffset == len(text) {
				t.Errorf("edit spans the whole file ([0, %d)), want it confined to the changed lines", len(text))
			}
		})
	}
}

func TestUnusedImportFix_SoleImport(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nimport \"fmt\"\n\nfunc F() int {\n\treturn 1\n}\n"
	offset := strings.Index(src, `"fmt"`)

	action, ok, err := langfeat.UnusedImportFix(file, []byte(src), offset)
	if err != nil {
		t.Fatalf("UnusedImportFix: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	got := applyEdits(src, action.Edits)
	want := "package foo\n\nfunc F() int {\n\treturn 1\n}\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestUnusedImportFix_OneOfMany(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\nfunc F() {\n\tfmt.Println(\"hi\")\n}\n"
	offset := strings.Index(src, `"strings"`)

	action, ok, err := langfeat.UnusedImportFix(file, []byte(src), offset)
	if err != nil {
		t.Fatalf("UnusedImportFix: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	got := applyEdits(src, action.Edits)
	want := "package foo\n\nimport (\n\t\"fmt\"\n)\n\nfunc F() {\n\tfmt.Println(\"hi\")\n}\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestUnusedImportFix_NotAnImport(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F() int {\n\treturn 1\n}\n"
	offset := strings.Index(src, "return")

	_, ok, err := langfeat.UnusedImportFix(file, []byte(src), offset)
	if err != nil {
		t.Fatalf("UnusedImportFix: %v", err)
	}
	if ok {
		t.Error("ok = true for a non-import offset, want false")
	}
}

func TestUnusedVarFix_ShortDeclSoleVar(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F() {\n\tx := 1\n\t_ = 2\n}\n"
	offset := strings.Index(src, "x := 1")

	action, ok, err := langfeat.UnusedVarFix(file, []byte(src), offset)
	if err != nil {
		t.Fatalf("UnusedVarFix: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	got := applyEdits(src, action.Edits)
	want := "package foo\n\nfunc F() {\n\t_ = 1\n\t_ = 2\n}\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestUnusedVarFix_ShortDeclMultiLHS(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F() {\n\ta, err := foo()\n\t_ = err\n}\n\nfunc foo() (int, error) { return 0, nil }\n"
	offset := strings.Index(src, "a, err")

	action, ok, err := langfeat.UnusedVarFix(file, []byte(src), offset)
	if err != nil {
		t.Fatalf("UnusedVarFix: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	got := applyEdits(src, action.Edits)
	want := "package foo\n\nfunc F() {\n\t_, err := foo()\n\t_ = err\n}\n\nfunc foo() (int, error) { return 0, nil }\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestUnusedVarFix_VarDecl(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F() {\n\tvar x int\n\t_ = 0\n}\n"
	offset := strings.Index(src, "x int")

	action, ok, err := langfeat.UnusedVarFix(file, []byte(src), offset)
	if err != nil {
		t.Fatalf("UnusedVarFix: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	got := applyEdits(src, action.Edits)
	want := "package foo\n\nfunc F() {\n\tvar _ int\n\t_ = 0\n}\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestUnusedVarFix_RangeLoopVarNotHandled(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F(m map[string]int) {\n\tfor k, v := range m {\n\t\t_ = k\n\t\t_ = v\n\t}\n}\n"
	offset := strings.Index(src, "v := range")

	_, ok, err := langfeat.UnusedVarFix(file, []byte(src), offset)
	if err != nil {
		t.Fatalf("UnusedVarFix: %v", err)
	}
	if ok {
		t.Error("ok = true for a range-clause variable, want false (not handled)")
	}
}

func TestAddImportFix_SingleCandidate(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F() {\n\tFoo()\n}\n"
	offset := strings.Index(src, "Foo()")

	actions, err := langfeat.AddImportFix(file, []byte(src), offset, "Foo", []langfeat.ImportCandidate{
		{PackageName: "bar", ImportPath: "example.com/bar"},
	})
	if err != nil {
		t.Fatalf("AddImportFix: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("len(actions) = %d, want 1", len(actions))
	}
	got := actions[0].Edits[0].NewText
	if !strings.Contains(got, `"example.com/bar"`) {
		t.Errorf("result = %q, want it to import example.com/bar", got)
	}
	if !strings.Contains(got, "bar.Foo()") {
		t.Errorf("result = %q, want it to qualify Foo as bar.Foo", got)
	}
}

func TestAddImportFix_MultipleCandidates(t *testing.T) {
	file := t.TempDir() + "/foo.go"
	src := "package foo\n\nfunc F() {\n\tFoo()\n}\n"
	offset := strings.Index(src, "Foo()")

	actions, err := langfeat.AddImportFix(file, []byte(src), offset, "Foo", []langfeat.ImportCandidate{
		{PackageName: "bar", ImportPath: "example.com/bar"},
		{PackageName: "baz", ImportPath: "example.com/baz"},
	})
	if err != nil {
		t.Fatalf("AddImportFix: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("len(actions) = %d, want 2", len(actions))
	}
	if !strings.Contains(actions[0].Edits[0].NewText, `"example.com/bar"`) {
		t.Errorf("actions[0] = %q, want example.com/bar", actions[0].Edits[0].NewText)
	}
	if !strings.Contains(actions[1].Edits[0].NewText, `"example.com/baz"`) {
		t.Errorf("actions[1] = %q, want example.com/baz", actions[1].Edits[0].NewText)
	}
}
