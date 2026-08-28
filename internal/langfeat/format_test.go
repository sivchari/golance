package langfeat_test

import (
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
)

func TestFormat(t *testing.T) {
	src := "package foo\nfunc F() int {\nreturn 1\n}\n"
	got, err := langfeat.Format([]byte(src))
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	want := "package foo\n\nfunc F() int {\n\treturn 1\n}\n"
	if string(got) != want {
		t.Errorf("Format(%q) = %q, want %q", src, got, want)
	}
}

func TestOrganizeImports_AddsMissing(t *testing.T) {
	src := "package foo\n\nfunc F() {\n\tfmt.Println(\"hi\")\n}\n"
	got, err := langfeat.OrganizeImports(t.TempDir()+"/foo.go", []byte(src))
	if err != nil {
		t.Fatalf("OrganizeImports: %v", err)
	}
	if !strings.Contains(string(got), `"fmt"`) {
		t.Errorf("OrganizeImports(%q) = %q, want it to add the fmt import", src, got)
	}
}

func TestOrganizeImports_RemovesUnused(t *testing.T) {
	src := "package foo\n\nimport \"strings\"\n\nfunc F() int {\n\treturn 1\n}\n"
	got, err := langfeat.OrganizeImports(t.TempDir()+"/foo.go", []byte(src))
	if err != nil {
		t.Fatalf("OrganizeImports: %v", err)
	}
	if strings.Contains(string(got), `"strings"`) {
		t.Errorf("OrganizeImports(%q) = %q, want the unused strings import removed", src, got)
	}
}
