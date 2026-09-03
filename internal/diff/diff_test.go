package diff_test

import (
	"fmt"
	"testing"

	"github.com/sivchari/golance/internal/diff"
)

// apply reconstructs "after" by applying edits (as returned by diff.Lines)
// to before — the round-trip diff.Lines(before, after) satisfies:
// apply(diff.Lines(before, after), before) == after, this file's central
// property.
func apply(t *testing.T, before []byte, edits []diff.Edit) []byte {
	t.Helper()
	out := make([]byte, 0, len(before))
	last := 0
	for i, e := range edits {
		if e.Start < last {
			t.Fatalf("edits[%d].Start = %d, before edits[%d]'s own end %d (edits must be sorted, non-overlapping)", i, e.Start, i-1, last)
		}
		if e.End < e.Start || e.End > len(before) {
			t.Fatalf("edits[%d] = %+v out of bounds for a %d-byte before text", i, e, len(before))
		}
		out = append(out, before[last:e.Start]...)
		out = append(out, e.New...)
		last = e.End
	}
	out = append(out, before[last:]...)
	return out
}

func TestLines_RoundTrip(t *testing.T) {
	tests := []struct {
		name          string
		before, after string
	}{
		{"no_op_empty", "", ""},
		{"no_op_content", "package p\n\nfunc F() {}\n", "package p\n\nfunc F() {}\n"},
		{"leading_change", "import(\n\"fmt\"\n)\n\nfunc F() {}\n", "import (\n\t\"fmt\"\n)\n\nfunc F() {}\n"},
		{"trailing_change", "func F() {}\nfunc G(){\n}\n", "func F() {}\nfunc G() {\n}\n"},
		{"middle_of_file", "func A() {}\nfunc B(){\n}\nfunc C() {}\n", "func A() {}\nfunc B() {\n}\nfunc C() {}\n"},
		{"pure_insertion", "package p\n\nfunc F() {}\n", "package p\n\nfunc F() {\n\treturn\n}\n"},
		{"pure_deletion", "package p\n\nfunc F() {\n\n\n}\n", "package p\n\nfunc F() {\n}\n"},
		{"trailing_newline_added", "package p", "package p\n"},
		{"trailing_newline_removed", "package p\n", "package p"},
		{"multibyte_utf16_content", "// 日本語コメント\nfunc F(){}\n", "// 日本語コメント\nfunc F() {}\n"},
		{"multiple_scattered_hunks", "func A(){\n}\nfunc B() {}\nfunc C(){\n}\nfunc D() {}\nfunc E(){\n}\n", "func A() {\n}\nfunc B() {}\nfunc C() {\n}\nfunc D() {}\nfunc E() {\n}\n"},
		{"entirely_different", "aaa\nbbb\nccc\n", "xxx\nyyy\nzzz\n"},
		{"empty_before", "", "package p\n"},
		{"empty_after", "package p\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := []byte(tt.before), []byte(tt.after)
			edits := diff.Lines(before, after)
			got := apply(t, before, edits)
			if string(got) != tt.after {
				t.Errorf("apply(Lines(before, after), before) = %q, want %q (edits: %+v)", got, tt.after, edits)
			}
		})
	}
}

func TestLines_NoOpReturnsNoEdits(t *testing.T) {
	src := []byte("package p\n\nfunc F() {}\n")
	edits := diff.Lines(src, src)
	if len(edits) != 0 {
		t.Errorf("Lines(src, src) = %+v, want no edits for identical input", edits)
	}
}

func TestLines_ScatteredHunksAreMinimal(t *testing.T) {
	// Two unrelated one-line changes, far apart in an otherwise-unchanged
	// file, should produce two small edits rather than one edit spanning
	// (or replacing) the whole file — the property that motivates this
	// package over a single whole-file replacement.
	before := []byte("line1\nline2\nline3\nline4\nline5\nline6\nline7\n")
	after := []byte("line1\nCHANGED2\nline3\nline4\nline5\nCHANGED6\nline7\n")
	edits := diff.Lines(before, after)
	if len(edits) != 2 {
		t.Fatalf("Lines returned %d edits, want exactly 2 for two scattered single-line changes: %+v", len(edits), edits)
	}
	for _, e := range edits {
		if e.End-e.Start > len("CHANGED2\n")+1 {
			t.Errorf("edit %+v spans more than one changed line, want a minimal per-line edit", e)
		}
	}
	got := apply(t, before, edits)
	if string(got) != string(after) {
		t.Errorf("apply(edits, before) = %q, want %q", got, after)
	}
}

func FuzzLines_RoundTrip(f *testing.F) {
	seeds := []struct{ before, after string }{
		{"", ""},
		{"a\nb\nc\n", "a\nx\nc\n"},
		{"a\nb\n", "b\na\n"},
		{"foo\n", "foo\nbar\n"},
	}
	for _, s := range seeds {
		f.Add(s.before, s.after)
	}
	f.Fuzz(func(t *testing.T, before, after string) {
		edits := diff.Lines([]byte(before), []byte(after))
		got := apply(t, []byte(before), edits)
		if string(got) != after {
			t.Errorf("apply(Lines(before, after), before) = %q, want %q (before=%q edits=%+v)", got, after, before, edits)
		}
	})
}

func ExampleLines() {
	before := []byte("func F(){\n}\n")
	after := []byte("func F() {\n}\n")
	edits := diff.Lines(before, after)
	fmt.Println(len(edits))
	// Output: 1
}
