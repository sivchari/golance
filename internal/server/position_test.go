package server

import (
	"testing"
	"unicode/utf8"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/overlay"
)

// asciiText, jpText, and emojiText exercise, respectively: plain ASCII, a
// multi-byte (3-byte UTF-8, 1 UTF-16 unit) script, and an astral character
// that needs a UTF-16 surrogate pair.
const (
	asciiText = "package main\n\nfunc main() {}\n"
	jpText    = "package main\n\n// こんにちは world\nfunc main() {}\n"
	emojiText = "package main\n\n// 🎉party\nfunc main() {}\n"
)

func TestByteOffsetForPosition(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		pos    protocol.Position
		want   int
		wantOK bool
	}{
		{"start of file", asciiText, protocol.Position{Line: 0, Character: 0}, 0, true},
		{"mid line ascii", asciiText, protocol.Position{Line: 2, Character: 5}, 19, true},
		{"end of file", asciiText, protocol.Position{Line: 3, Character: 0}, len(asciiText), true},
		{"line out of range", asciiText, protocol.Position{Line: 99, Character: 0}, 0, false},
		{"character out of range", asciiText, protocol.Position{Line: 0, Character: 99}, 0, false},

		// jpText: line 2 is "// こんにちは world" — こんにちは is 5 runes,
		// each 3 bytes in UTF-8 and 1 unit in UTF-16.
		{"before jp run", jpText, protocol.Position{Line: 2, Character: 3}, 17, true},
		{"after jp run", jpText, protocol.Position{Line: 2, Character: 8}, 32, true},
		{"inside jp run midpoint", jpText, protocol.Position{Line: 2, Character: 5}, 23, true},

		// emojiText: line 2 is "// 🎉party" — 🎉 (U+1F389) is 4 bytes in
		// UTF-8 and a 2-unit UTF-16 surrogate pair.
		{"before emoji", emojiText, protocol.Position{Line: 2, Character: 3}, 17, true},
		{"after emoji (surrogate pair consumed)", emojiText, protocol.Position{Line: 2, Character: 5}, 21, true},
		{"inside emoji surrogate pair", emojiText, protocol.Position{Line: 2, Character: 4}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := byteOffsetForPosition([]byte(tt.text), tt.pos)
			if ok != tt.wantOK {
				t.Fatalf("byteOffsetForPosition(%+v) ok = %v, want %v", tt.pos, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("byteOffsetForPosition(%+v) = %d, want %d", tt.pos, got, tt.want)
			}
		})
	}
}

// TestByteOffsetForPositionRoundTrip checks that byteOffsetForPosition is
// the exact inverse of overlay.UTF16PositionForByteOffset at every
// rune-boundary byte offset in each fixture. Offsets that land inside a
// multi-byte rune are intentionally excluded: both functions resolve such
// an offset up to the next whole rune, so a mid-rune offset and the
// following rune-boundary offset can legitimately map to the same
// Position, breaking exact round-tripping — that never happens for real
// callers, who only ever pass rune-boundary offsets (e.g. from go/token
// positions).
func TestByteOffsetForPositionRoundTrip(t *testing.T) {
	for _, text := range []string{asciiText, jpText, emojiText} {
		text := text
		for offset := 0; offset <= len(text); {
			pos, ok := overlay.UTF16PositionForByteOffset([]byte(text), offset)
			if !ok {
				t.Fatalf("UTF16PositionForByteOffset(%d) unexpectedly failed", offset)
			}
			got, ok := byteOffsetForPosition([]byte(text), pos)
			if !ok {
				t.Fatalf("byteOffsetForPosition(%+v) (from offset %d) failed", pos, offset)
			}
			if got != offset {
				t.Fatalf("round trip offset %d -> %+v -> %d, want %d", offset, pos, got, offset)
			}
			if offset == len(text) {
				break
			}
			_, size := utf8.DecodeRune([]byte(text)[offset:])
			offset += size
		}
	}
}

func TestLineColForByteOffset(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		offset    int
		line, col uint32
	}{
		{"start of file", asciiText, 0, 1, 1},
		{"start of second line", asciiText, 13, 2, 1},
		{"mid line", asciiText, 18, 3, 5},
		{"end of file", asciiText, len(asciiText), 4, 1},
		{"inside jp run (byte offset, not rune)", jpText, 20, 3, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, col := lineColForByteOffset([]byte(tt.text), tt.offset)
			if line != tt.line || col != tt.col {
				t.Fatalf("lineColForByteOffset(%d) = (%d,%d), want (%d,%d)", tt.offset, line, col, tt.line, tt.col)
			}
		})
	}
}

func TestByteOffsetForLineCol(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		line, col uint32
		want      int
		wantOK    bool
	}{
		{"start of file", asciiText, 1, 1, 0, true},
		{"start of second line", asciiText, 2, 1, 13, true},
		{"mid line", asciiText, 3, 5, 18, true},
		{"end of last line", asciiText, 4, 1, len(asciiText), true},
		{"line zero invalid", asciiText, 0, 1, 0, false},
		{"col zero invalid", asciiText, 1, 0, 0, false},
		{"line out of range", asciiText, 99, 1, 0, false},
		{"col past end of line", asciiText, 1, 99, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := byteOffsetForLineCol([]byte(tt.text), tt.line, tt.col)
			if ok != tt.wantOK {
				t.Fatalf("byteOffsetForLineCol(%d,%d) ok = %v, want %v", tt.line, tt.col, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("byteOffsetForLineCol(%d,%d) = %d, want %d", tt.line, tt.col, got, tt.want)
			}
		})
	}
}

// TestLineColByteOffsetRoundTrip checks that byteOffsetForLineCol and
// lineColForByteOffset are exact inverses at every valid byte offset,
// including inside multi-byte runes (byte columns, unlike UTF-16
// character offsets, are valid at any byte position).
func TestLineColByteOffsetRoundTrip(t *testing.T) {
	for _, text := range []string{asciiText, jpText, emojiText} {
		text := text
		for offset := 0; offset <= len(text); offset++ {
			line, col := lineColForByteOffset([]byte(text), offset)
			got, ok := byteOffsetForLineCol([]byte(text), line, col)
			if !ok {
				t.Fatalf("byteOffsetForLineCol(%d,%d) (from offset %d) failed", line, col, offset)
			}
			if got != offset {
				t.Fatalf("round trip offset %d -> (%d,%d) -> %d, want %d", offset, line, col, got, offset)
			}
		}
	}
}

func TestPositionToXref(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		pos       protocol.Position
		line, col int
		wantOK    bool
	}{
		{"ascii mid line", asciiText, protocol.Position{Line: 2, Character: 5}, 3, 6, true},
		{"jp run", jpText, protocol.Position{Line: 2, Character: 8}, 3, 19, true},
		{"out of range", asciiText, protocol.Position{Line: 99, Character: 0}, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, col, ok := positionToXref([]byte(tt.text), tt.pos)
			if ok != tt.wantOK {
				t.Fatalf("positionToXref(%+v) ok = %v, want %v", tt.pos, ok, tt.wantOK)
			}
			if ok && (line != tt.line || col != tt.col) {
				t.Fatalf("positionToXref(%+v) = (%d,%d), want (%d,%d)", tt.pos, line, col, tt.line, tt.col)
			}
		})
	}
}

func TestXrefRangeToLSP(t *testing.T) {
	// "func main() {}" starts at byte 18 on line 3 (1-based) of asciiText,
	// column 1; "main" spans columns 6..10 (1-based, exclusive end).
	rng, ok := xrefRangeToLSP([]byte(asciiText), 3, 6, 10)
	if !ok {
		t.Fatalf("xrefRangeToLSP: ok = false")
	}
	want := protocol.Range{
		Start: protocol.Position{Line: 2, Character: 5},
		End:   protocol.Position{Line: 2, Character: 9},
	}
	if rng != want {
		t.Fatalf("xrefRangeToLSP = %+v, want %+v", rng, want)
	}

	if _, ok := xrefRangeToLSP([]byte(asciiText), 99, 1, 2); ok {
		t.Fatalf("xrefRangeToLSP: expected ok=false for out-of-range line")
	}
}

func TestOffsetRangeToLSP(t *testing.T) {
	rng, ok := offsetRangeToLSP([]byte(asciiText), 19, 23)
	if !ok {
		t.Fatalf("offsetRangeToLSP: ok = false")
	}
	want := protocol.Range{
		Start: protocol.Position{Line: 2, Character: 5},
		End:   protocol.Position{Line: 2, Character: 9},
	}
	if rng != want {
		t.Fatalf("offsetRangeToLSP = %+v, want %+v", rng, want)
	}

	if _, ok := offsetRangeToLSP([]byte(asciiText), -1, 2); ok {
		t.Fatalf("offsetRangeToLSP: expected ok=false for negative offset")
	}
}
