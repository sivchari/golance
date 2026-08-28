package langfeat_test

import (
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestHover_Func(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "hover", "hover.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "func Add") + len("func ")

	got, err := langfeat.Hover(cp, path, offset)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got == nil {
		t.Fatal("Hover returned nil, want a result")
	}
	if !strings.Contains(got.Signature, "func Add(a int, b int) int") {
		t.Errorf("Signature = %q, want it to contain the Add signature", got.Signature)
	}
	if !strings.Contains(got.Doc, "Add returns the sum of a and b.") {
		t.Errorf("Doc = %q, want the Add doc comment", got.Doc)
	}
}

func TestHover_Type(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "hover", "hover.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "type Greeting") + len("type ")

	got, err := langfeat.Hover(cp, path, offset)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got == nil {
		t.Fatal("Hover returned nil, want a result")
	}
	if !strings.Contains(got.Signature, "Greeting") || !strings.Contains(got.Signature, "struct") {
		t.Errorf("Signature = %q, want a Greeting struct signature", got.Signature)
	}
	if !strings.Contains(got.Doc, "friendly message") {
		t.Errorf("Doc = %q, want the Greeting doc comment", got.Doc)
	}
}

func TestHover_NoIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "hover", "hover.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Offset inside the whitespace between two declarations: no identifier.
	offset := mustIndex(t, text, "\n\n// Greeting")

	got, err := langfeat.Hover(cp, path, offset)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got != nil {
		t.Errorf("Hover = %+v, want nil (no identifier at offset)", got)
	}
}

// mustIndex returns the byte offset of the first occurrence of substr in
// text, failing the test if substr is not found.
func mustIndex(t *testing.T, text []byte, substr string) int {
	t.Helper()
	i := strings.Index(string(text), substr)
	if i < 0 {
		t.Fatalf("substring %q not found in:\n%s", substr, text)
	}
	return i
}
