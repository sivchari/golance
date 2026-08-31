package langfeat_test

import (
	"slices"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
)

// tok is a shorthand for building a langfeat.Token in these tests.
func tok(start, end int, kind langfeat.TokenKind, mods langfeat.TokenModifier) langfeat.Token {
	return langfeat.Token{Range: langfeat.Range{StartOffset: start, EndOffset: end}, Kind: kind, Modifiers: mods}
}

func TestEncode_Empty(t *testing.T) {
	got := langfeat.Encode([]byte("anything"), nil)
	if len(got) != 0 {
		t.Errorf("Encode(nil tokens) = %v, want an empty slice", got)
	}
}

func TestEncode_SingleToken(t *testing.T) {
	got := langfeat.Encode([]byte("abc"), []langfeat.Token{
		tok(0, 3, langfeat.TokenKeyword, langfeat.ModDefinition),
	})
	want := []uint32{0, 0, 3, uint32(langfeat.TokenKeyword), uint32(langfeat.ModDefinition)}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_SameLineDeltaChar verifies deltaStartChar is relative to the
// previous token's start character when both tokens are on the same line,
// including the zero-gap case of two tokens with no space between them
// ("a+b": ident, operator, ident, each immediately following the last).
func TestEncode_SameLineDeltaChar(t *testing.T) {
	got := langfeat.Encode([]byte("a+b"), []langfeat.Token{
		tok(0, 1, langfeat.TokenVariable, 0),
		tok(1, 2, langfeat.TokenOperator, 0),
		tok(2, 3, langfeat.TokenVariable, 0),
	})
	want := []uint32{
		0, 0, 1, uint32(langfeat.TokenVariable), 0,
		0, 1, 1, uint32(langfeat.TokenOperator), 0,
		0, 1, 1, uint32(langfeat.TokenVariable), 0,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_SameLineDeltaCharWithGap verifies deltaStartChar accounts for a
// gap (untokenized punctuation/whitespace) between two same-line tokens.
func TestEncode_SameLineDeltaCharWithGap(t *testing.T) {
	got := langfeat.Encode([]byte("ab cd"), []langfeat.Token{
		tok(0, 2, langfeat.TokenVariable, 0),
		tok(3, 5, langfeat.TokenVariable, 0),
	})
	want := []uint32{
		0, 0, 2, uint32(langfeat.TokenVariable), 0,
		0, 3, 2, uint32(langfeat.TokenVariable), 0,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_NewLineResetsChar verifies that when a token starts on a later
// line than the previous one, deltaLine is the line difference and
// deltaStartChar is the new token's absolute column (not relative to the
// previous token's column).
func TestEncode_NewLineResetsChar(t *testing.T) {
	got := langfeat.Encode([]byte("ab\ncd"), []langfeat.Token{
		tok(0, 2, langfeat.TokenVariable, 0),
		tok(3, 5, langfeat.TokenVariable, 0),
	})
	want := []uint32{
		0, 0, 2, uint32(langfeat.TokenVariable), 0,
		1, 0, 2, uint32(langfeat.TokenVariable), 0,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_ThreeLinesEachDeltaLineOne verifies deltaLine is computed
// fresh for each token (not accumulated incorrectly) across more than two
// lines, and that a token starting a line always gets deltaStartChar equal
// to its own absolute column, regardless of the previous line's column.
func TestEncode_ThreeLinesEachDeltaLineOne(t *testing.T) {
	got := langfeat.Encode([]byte("a\nb\nc"), []langfeat.Token{
		tok(0, 1, langfeat.TokenKeyword, 0),
		tok(2, 3, langfeat.TokenKeyword, 0),
		tok(4, 5, langfeat.TokenKeyword, 0),
	})
	want := []uint32{
		0, 0, 1, uint32(langfeat.TokenKeyword), 0,
		1, 0, 1, uint32(langfeat.TokenKeyword), 0,
		1, 0, 1, uint32(langfeat.TokenKeyword), 0,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_TokenNotAtLineStart verifies a token's start column correctly
// accounts for the bytes preceding it on its own line, even when that
// preceding text is not itself a token (plain whitespace here).
func TestEncode_TokenNotAtLineStart(t *testing.T) {
	got := langfeat.Encode([]byte("  x"), []langfeat.Token{
		tok(2, 3, langfeat.TokenVariable, 0),
	})
	want := []uint32{0, 2, 1, uint32(langfeat.TokenVariable), 0}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_MultibyteBMP verifies UTF-16 length and column counting for
// multi-byte-in-UTF-8-but-single-UTF-16-unit runes (each kanji here is 3
// UTF-8 bytes but exactly 1 UTF-16 code unit), the case that would silently
// break if Encode counted bytes or runes instead of UTF-16 units.
func TestEncode_MultibyteBMP(t *testing.T) {
	text := []byte("日本語 x") // 3 kanji (3 bytes each, 1 UTF-16 unit each), a space, "x"
	xOffset := 10           // 3*3 (kanji) + 1 (space)
	got := langfeat.Encode(text, []langfeat.Token{
		tok(0, 9, langfeat.TokenComment, 0), // the three kanji
		tok(xOffset, xOffset+1, langfeat.TokenVariable, 0),
	})
	want := []uint32{
		0, 0, 3, uint32(langfeat.TokenComment), 0,
		// deltaStartChar: the kanji token starts at UTF-16 char 0 and is 3
		// units long (char 3), the space is 1 more unit (char 4) = "x"'s
		// start char; deltaStartChar is relative to the previous token's
		// *start* char (0), so 4 - 0 = 4.
		0, 4, 1, uint32(langfeat.TokenVariable), 0,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_SurrogatePair verifies a rune outside the Basic Multilingual
// Plane (needing a UTF-16 surrogate pair) counts as 2 UTF-16 units, both
// for a token's own length and for the column of whatever follows it. This
// is the case most likely to silently misalign highlighting if gotten
// wrong, since it differs from both the byte count (4) and the rune count
// (1).
func TestEncode_SurrogatePair(t *testing.T) {
	text := []byte("😀x") // U+1F600, 4 UTF-8 bytes, 2 UTF-16 units
	got := langfeat.Encode(text, []langfeat.Token{
		tok(0, 4, langfeat.TokenString, 0),
		tok(4, 5, langfeat.TokenVariable, 0),
	})
	want := []uint32{
		0, 0, 2, uint32(langfeat.TokenString), 0,
		0, 2, 1, uint32(langfeat.TokenVariable), 0,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Encode = %v, want %v", got, want)
	}
}

// TestEncode_ModifiersPassThrough verifies Encode passes a combined
// modifier bitset through unchanged as the fifth uint32.
func TestEncode_ModifiersPassThrough(t *testing.T) {
	mods := langfeat.ModDefinition | langfeat.ModStatic | langfeat.ModReadonly
	got := langfeat.Encode([]byte("abc"), []langfeat.Token{
		tok(0, 3, langfeat.TokenVariable, mods),
	})
	if got[4] != uint32(mods) {
		t.Errorf("modifiers = %d, want %d (definition|static|readonly)", got[4], uint32(mods))
	}
}
