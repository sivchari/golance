package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func hasLabel(items []langfeat.CompletionItem, label string) bool {
	for _, it := range items {
		if it.Label == label {
			return true
		}
	}
	return false
}

func TestCompletion_Selector(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completion", "completion.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return p.X") + len("return p.")

	items, err := langfeat.Completion(cp, text, path, offset)
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	for _, want := range []string{"Add", "Scale", "X", "Y"} {
		if !hasLabel(items, want) {
			t.Errorf("Completion results = %+v, want to contain %q", items, want)
		}
	}
}

func TestCompletion_SelectorPrefixFilter(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completion", "completion.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return p.Sc") + len("return p.Sc")

	items, err := langfeat.Completion(cp, text, path, offset)
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	if len(items) != 1 || items[0].Label != "Scale" {
		t.Errorf("Completion results = %+v, want exactly [Scale]", items)
	}
}

func TestCompletion_PackageMember(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completion", "completion.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "strings.ToUpper") + len("strings.")

	items, err := langfeat.Completion(cp, text, path, offset)
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	if !hasLabel(items, "ToUpper") {
		t.Errorf("Completion results = %+v, want to contain ToUpper", items)
	}
	if !hasLabel(items, "Split") {
		t.Errorf("Completion results = %+v, want to contain Split (exported strings member)", items)
	}
}

func TestCompletion_Lexical(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completion", "completion.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return tot") + len("return tot")

	items, err := langfeat.Completion(cp, text, path, offset)
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	if !hasLabel(items, "total") {
		t.Errorf("Completion results = %+v, want to contain local var total", items)
	}
	if hasLabel(items, "origin") {
		t.Errorf("Completion results = %+v, want origin filtered out (does not match prefix tot)", items)
	}
}

// TestCompletion_BrokenSelector covers (d): a dangling selector ("f." with
// nothing typed after the dot yet) still resolves x's type, because
// parser.AllErrors keeps the SelectorExpr for "f." in the partial AST (with
// an empty Sel) and go/types still records f's static type. Selector
// completion therefore works even mid-syntax-error, for this shape of
// brokenness.
func TestCompletion_BrokenSelector(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "broken", "broken.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "f.\n") + len("f.")

	items, err := langfeat.Completion(cp, text, path, offset)
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	if !hasLabel(items, "Bar") {
		t.Errorf("Completion results = %+v, want Foo's Bar field resolved despite the dangling selector", items)
	}
}

// TestCompletion_BrokenPackageSelector is TestCompletion_BrokenSelector's
// package-member counterpart: "strings." with nothing typed after the dot
// yet still resolves to strings' exported members.
func TestCompletion_BrokenPackageSelector(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "broken", "broken.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "strings.\n") + len("strings.")

	items, err := langfeat.Completion(cp, text, path, offset)
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	if !hasLabel(items, "ToUpper") {
		t.Errorf("Completion results = %+v, want strings.ToUpper resolved despite the dangling selector", items)
	}
}

// TestCompletion_OffsetPastEndOfText is a regression test for the panic
// trigger the investigation confirmed: a completion request's offset is
// validated against one overlay read (see checkedFile in internal/server),
// but Completion re-reads the overlay itself afterward. A didChange that
// shrinks the buffer in between can leave offset past the freshly-read
// text's length, which used to panic with "slice bounds out of range".
func TestCompletion_OffsetPastEndOfText(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "completion", "completion.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if _, err := langfeat.Completion(cp, text, path, len(text)+1000); err != nil {
		t.Fatalf("Completion() with an out-of-range offset error = %v, want no error (and no panic)", err)
	}
}

// TestCompletion_UnresolvedSelectorBaseReturnsNilButUnimportedRecoversIt
// documents the resolution of Finding L3: "fmt.Sp" with fmt never imported
// leaves sel.X ("fmt") with no recorded type at all (unlike a broken
// selector whose base merely has an invalid type, e.g. calling an undefined
// function — go/types still records Typ[Invalid] for that case, which
// selectorCompletions already handles by returning zero members rather than
// nil). Completion's own selectorCompletions has no graph access and so
// cannot itself tell "fmt" apart from any other undeclared identifier (see
// its own doc); it correctly returns nil here. That is not a dead end,
// though: Unimported reports this exact shape as a shape-2 candidate (see
// TestUnimported_SelectorUnresolvedPackage), which the server layer
// (appendUnimportedCompletions) resolves into real candidates from the
// workspace graph — verified directly against gopls v0.23.0, which answers
// the identical broken-selector position with its own equivalent
// unimported-package fallback rather than an empty list.
func TestCompletion_UnresolvedSelectorBaseReturnsNilButUnimportedRecoversIt(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "fmt.Sp") + len("fmt.Sp")

	items, err := langfeat.Completion(cp, text, path, offset)
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("Completion(fmt.Sp, fmt unimported) = %+v, want none (langfeat has no graph access to resolve fmt itself)", items)
	}

	uctx, ok := langfeat.Unimported(cp, text, path, offset)
	if !ok || uctx.Selector != "fmt" {
		t.Fatalf("Unimported(fmt.Sp) = %+v, ok=%v, want {Selector: fmt, ...}, ok=true — the server layer's fallback that actually answers this position", uctx, ok)
	}
}

func TestMergeUnimported(t *testing.T) {
	items := []langfeat.CompletionItem{{Label: "A"}}
	candidates := []langfeat.CompletionItem{{Label: "B"}}
	got := langfeat.MergeUnimported(items, candidates)
	if len(got) != 2 || got[0].Label != "A" || got[1].Label != "B" {
		t.Errorf("MergeUnimported = %+v, want [A B]", got)
	}
}
