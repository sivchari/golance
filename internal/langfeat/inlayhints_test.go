package langfeat_test

import (
	"math"
	"slices"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// wholeFile is a byte offset range wide enough to cover any testdata file
// used in this package, for tests that do not exercise range filtering.
const wholeFile = math.MaxInt32

// hintsOfKind returns the subset of hints whose Kind is kind, in the order
// InlayHints returned them (source order).
func hintsOfKind(hints []langfeat.Hint, kind langfeat.HintKind) []langfeat.Hint {
	var out []langfeat.Hint
	for _, h := range hints {
		if h.Kind == kind {
			out = append(out, h)
		}
	}
	return out
}

// labelsOf returns hints' Labels, in order.
func labelsOf(hints []langfeat.Hint) []string {
	labels := make([]string, len(hints))
	for i, h := range hints {
		labels[i] = h.Label
	}
	return labels
}

func TestInlayHints_AssignVariableTypes(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "inlay.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}
	if len(hints) != 2 {
		t.Fatalf("InlayHints = %+v, want 2 hints", hints)
	}
	if hints[0].Label != ": int" || hints[0].Kind != langfeat.AssignVariableTypes || hints[0].Render != langfeat.RenderType {
		t.Errorf("hints[0] = %+v, want label %q, kind AssignVariableTypes, render RenderType", hints[0], ": int")
	}
	if hints[1].Label != ": string" {
		t.Errorf("hints[1].Label = %q, want %q", hints[1].Label, ": string")
	}
	if hints[0].Offset >= hints[1].Offset {
		t.Errorf("hints not in source order: %+v", hints)
	}
}

func TestInlayHints_ParameterNames(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "params.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	got := labelsOf(hintsOfKind(hints, langfeat.ParameterNames))
	want := []string{"x:", "y:", "prefix:", "nums...:"}
	if !slices.Equal(got, want) {
		t.Errorf("parameterNames labels = %v, want %v (addNamed(x, y) suppressed because both args match their parameter names; addNamed(y, x) and sum3's variadic call are not)", got, want)
	}
}

func TestInlayHints_RangeVariableTypes(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "ranges.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	got := labelsOf(hintsOfKind(hints, langfeat.RangeVariableTypes))
	want := []string{": string", ": int"}
	if !slices.Equal(got, want) {
		t.Errorf("rangeVariableTypes labels = %v, want %v (k is string, v is int)", got, want)
	}
}

func TestInlayHints_CompositeLiteralFields(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "composite.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	got := labelsOf(hintsOfKind(hints, langfeat.CompositeLiteralFields))
	// unkeyedPoint's Point{1, 2}, pointSlice's two elided-type {1, 2} and
	// {3, 4} elements, and pointerSlice's elided-type {1, 2} element: each
	// unkeyed struct literal contributes an "X:" / "Y:" pair.
	want := []string{"X:", "Y:", "X:", "Y:", "X:", "Y:", "X:", "Y:"}
	if !slices.Equal(got, want) {
		t.Errorf("compositeLiteralFields labels = %v, want %v", got, want)
	}
}

func TestInlayHints_CompositeLiteralTypes(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "composite.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	got := labelsOf(hintsOfKind(hints, langfeat.CompositeLiteralTypes))
	// unkeyedPoint's Point{1, 2} has an explicit type, so it gets no hint.
	// pointSlice's two elided elements get "Point"; pointerSlice's elided
	// element is implicitly a *Point, so it gets "&Point".
	want := []string{"Point", "Point", "&Point"}
	if !slices.Equal(got, want) {
		t.Errorf("compositeLiteralTypes labels = %v, want %v", got, want)
	}
}

func TestInlayHints_ConstantValues(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "consts.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	got := labelsOf(hintsOfKind(hints, langfeat.ConstantValues))
	want := []string{"= 0", "= 1", "= 2"}
	if !slices.Equal(got, want) {
		t.Errorf("constantValues labels = %v, want %v (iota values for Sunday, Monday, Tuesday; Greeting is already a literal and gets no hint)", got, want)
	}
}

func TestInlayHints_FunctionTypeParameters(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "generics.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	got := hintsOfKind(hints, langfeat.FunctionTypeParameters)
	if len(got) != 1 || got[0].Label != "[int]" {
		t.Errorf("functionTypeParameters hints = %+v, want a single %q hint for Identity(42)", got, "[int]")
	}
}

func TestInlayHints_PackageQualifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "qualifier.go")

	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	got := labelsOf(hintsOfKind(hints, langfeat.AssignVariableTypes))
	want := []string{": Local", ": typedefdep.Remote"}
	if !slices.Equal(got, want) {
		t.Errorf("assignVariableTypes labels = %v, want %v (a same-package type renders unqualified, and a type from another package renders by its short package name only, never the full module path)", got, want)
	}
}

func TestInlayHints_DisabledKindIsOmitted(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "params.go")

	enabled := langfeat.ResolveHints(map[string]bool{"parameterNames": false})
	hints, err := langfeat.InlayHints(cp, path, 0, wholeFile, enabled)
	if err != nil {
		t.Fatalf("InlayHints: %v", err)
	}

	if got := hintsOfKind(hints, langfeat.ParameterNames); len(got) != 0 {
		t.Errorf("parameterNames hints = %+v, want none (disabled)", got)
	}
	if got := hintsOfKind(hints, langfeat.AssignVariableTypes); len(got) != 6 {
		t.Errorf("assignVariableTypes hints = %+v, want 6 (unaffected by disabling parameterNames)", got)
	}
}

func TestInlayHints_ResolveHints(t *testing.T) {
	allOn := langfeat.ResolveHints(nil)
	for _, k := range langfeat.AllHintKinds {
		if !allOn[k] {
			t.Errorf("ResolveHints(nil)[%s] = false, want true (every kind on by default)", k)
		}
	}

	partial := langfeat.ResolveHints(map[string]bool{"parameterNames": false, "constantValues": true})
	if partial[langfeat.ParameterNames] {
		t.Error("ResolveHints: parameterNames should be disabled when explicitly set to false")
	}
	if !partial[langfeat.ConstantValues] {
		t.Error("ResolveHints: constantValues should stay enabled when explicitly set to true")
	}
	if !partial[langfeat.AssignVariableTypes] {
		t.Error("ResolveHints: assignVariableTypes should default on when not mentioned")
	}
}

func TestInlayHints_RangeFiltersHints(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "inlay", "inlay.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	full, err := langfeat.InlayHints(cp, path, 0, wholeFile, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints (full range): %v", err)
	}
	if len(full) != 2 {
		t.Fatalf("InlayHints (full range) = %+v, want 2 hints", full)
	}

	// Narrow the range to end right before the "y := ..." statement, so
	// only the "x := 1" hint should survive.
	end := mustIndex(t, text, `y := "hello"`)
	narrowed, err := langfeat.InlayHints(cp, path, 0, end, langfeat.ResolveHints(nil))
	if err != nil {
		t.Fatalf("InlayHints (narrowed range): %v", err)
	}
	if len(narrowed) != 1 || narrowed[0].Label != ": int" {
		t.Errorf("InlayHints (narrowed range) = %+v, want exactly the %q hint", narrowed, ": int")
	}
}
