package langfeat_test

import (
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestSignatureHelp_ActiveParam(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "signature", "signature.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	call := "return Add(1, 2)"
	firstArg := mustIndex(t, text, call) + len("return Add(")
	secondArg := mustIndex(t, text, call) + len("return Add(1, ")

	for _, tc := range []struct {
		name   string
		offset int
		want   int
	}{
		{"first arg", firstArg, 0},
		{"second arg", secondArg, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := langfeat.SignatureHelp(cp, path, tc.offset)
			if err != nil {
				t.Fatalf("SignatureHelp: %v", err)
			}
			if got == nil {
				t.Fatal("SignatureHelp returned nil, want a result")
			}
			if len(got.Params) != 2 {
				t.Errorf("Params = %v, want 2 params", got.Params)
			}
			if got.ActiveParam != tc.want {
				t.Errorf("ActiveParam = %d, want %d", got.ActiveParam, tc.want)
			}
		})
	}
}

func TestSignatureHelp_VariadicCapsActiveParam(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "signature", "signature.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return Sum(1, 2, 3, 4)") + len("return Sum(1, 2, 3, ")

	got, err := langfeat.SignatureHelp(cp, path, offset)
	if err != nil {
		t.Fatalf("SignatureHelp: %v", err)
	}
	if got == nil {
		t.Fatal("SignatureHelp returned nil, want a result")
	}
	if len(got.Params) != 1 {
		t.Errorf("Params = %v, want 1 (variadic) param", got.Params)
	}
	if got.ActiveParam != 0 {
		t.Errorf("ActiveParam = %d, want 0 (capped at the sole variadic param)", got.ActiveParam)
	}
	if !strings.Contains(got.Label, "...int") {
		t.Errorf("Label = %q, want it to describe a variadic ...int param", got.Label)
	}
}
