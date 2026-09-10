package check

import (
	"go/token"
	"testing"
)

// posAt builds a token.Position carrying only what diagAt reads from it
// (Filename and Offset).
func posAt(filename string, offset int) token.Position {
	return token.Position{Filename: filename, Offset: offset}
}

func TestIdentEnd(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		offset int
		want   int
	}{
		{"identifier", "undefinedIdent\n", 0, len("undefinedIdent")},
		{"identifier_with_trailing_content", "foo(bar)", 0, len("foo")},
		{"keyword", "func F() {}", 0, len("func")},
		{"interpreted_string_literal", `"a string literal"`, 0, len(`"a string literal"`)},
		{"interpreted_string_literal_with_escape", `"a\"b"`, 0, len(`"a\"b"`)},
		{"raw_string_literal", "`raw\nstring`", 0, len("`raw\nstring`")},
		{"int_literal", "42", 0, len("42")},
		{"float_literal", "3.14", 0, len("3.14")},
		{"hex_int_literal", "0xFF", 0, len("0xFF")},
		{"char_literal", "'a'", 0, len("'a'")},
		{"mid_string_offset", `x := "hello"`, len("x := "), len(`"hello"`)},
		{"not_a_token_start", "  42", 0, 0}, // offset itself is whitespace, not a token start
		{"operator", "+", 0, 0},             // not identifier/literal/keyword: unchanged
		{"empty", "", 0, 0},
		{"offset_at_end", "42", 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identEnd([]byte(tt.text), tt.offset)
			want := tt.offset + tt.want
			if got != want {
				t.Errorf("identEnd(%q, %d) = %d, want %d", tt.text, tt.offset, got, want)
			}
		})
	}
}

// TestDiagAt_NeverZeroWidthForKnownTokens is a regression test for the bug
// diagnostics.go's own doc describes: identEnd used to extend the end
// position only for identifiers, so a diagnostic on a string literal, a
// number, or (via typeErrorRange) a composite literal or call expression
// came back zero-width (Start == End), which editors render as a caret
// instead of an underline.
func TestDiagAt_NeverZeroWidthForKnownTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		off  int
	}{
		{"string_literal", `var y int = "a string literal"`, len("var y int = ")},
		{"int_literal", `var z string = 42`, len("var z string = ")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := mapReader{"f.go": []byte(tt.text)}
			pos := posAt("f.go", tt.off)
			d, ok := diagAt(reader, pos, pos, "some type error", SeverityError)
			if !ok {
				t.Fatalf("diagAt returned ok=false")
			}
			if d.StartLine == d.EndLine && d.StartCol == d.EndCol {
				t.Errorf("diagAt produced a zero-width diagnostic: %+v", d)
			}
		})
	}
}
