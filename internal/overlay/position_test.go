package overlay

import (
	"testing"
	"unicode/utf8"

	"go.lsp.dev/protocol"
)

func TestByteOffsetForUTF16Position(t *testing.T) {
	tests := []struct {
		name string
		text string
		pos  protocol.Position
		want int
		ok   bool
	}{
		{"start of file", "hello\nworld", protocol.Position{Line: 0, Character: 0}, 0, true},
		{"mid first line", "hello\nworld", protocol.Position{Line: 0, Character: 3}, 3, true},
		{"end of first line", "hello\nworld", protocol.Position{Line: 0, Character: 5}, 5, true},
		{"start of second line", "hello\nworld", protocol.Position{Line: 1, Character: 0}, 6, true},
		{"end of second line, no trailing newline", "hello\nworld", protocol.Position{Line: 1, Character: 5}, 11, true},
		{"character past end of line", "hi\nbye", protocol.Position{Line: 0, Character: 5}, 0, false},
		{"line past end of file", "hi\nbye", protocol.Position{Line: 5, Character: 0}, 0, false},
		{"empty text at 0,0", "", protocol.Position{Line: 0, Character: 0}, 0, true},

		// Multibyte / surrogate-pair boundary cases.
		{"before multibyte char (3-byte é... actually 2-byte)", "café", protocol.Position{Line: 0, Character: 3}, 3, true},
		{"after multibyte char", "café", protocol.Position{Line: 0, Character: 4}, 5, true}, // 'é' is 2 bytes in UTF-8, 1 UTF-16 unit
		{"before astral char (surrogate pair)", "a\U0001F600b", protocol.Position{Line: 0, Character: 1}, 1, true},
		{"after astral char", "a\U0001F600b", protocol.Position{Line: 0, Character: 3}, 5, true}, // 😀 = 4 bytes UTF-8, 2 UTF-16 units
		{"inside astral char surrogate pair is invalid", "a\U0001F600b", protocol.Position{Line: 0, Character: 2}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := byteOffsetForUTF16Position([]byte(tt.text), tt.pos)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (got=%d)", ok, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Fatalf("offset = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestUTF16PositionForByteOffset(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		offset int
		want   protocol.Position
		ok     bool
	}{
		{"start", "hello\nworld", 0, protocol.Position{Line: 0, Character: 0}, true},
		{"mid first line", "hello\nworld", 3, protocol.Position{Line: 0, Character: 3}, true},
		{"start of second line", "hello\nworld", 6, protocol.Position{Line: 1, Character: 0}, true},
		{"end of text", "hello\nworld", 11, protocol.Position{Line: 1, Character: 5}, true},
		{"negative offset invalid", "hello", -1, protocol.Position{}, false},
		{"offset past end invalid", "hello", 6, protocol.Position{}, false},
		{"after astral char", "a\U0001F600b", 5, protocol.Position{Line: 0, Character: 3}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := UTF16PositionForByteOffset([]byte(tt.text), tt.offset)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Fatalf("position = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPositionConversionRoundTrip(t *testing.T) {
	text := "package main\n\nfunc main() {\n\tprintln(\"héllo, 世界 \U0001F600\")\n}\n"
	b := []byte(text)
	// Only rune-boundary offsets are meaningful positions (go/token
	// positions never split a rune); byteOffsetForUTF16Position already
	// rejects positions mid surrogate-pair separately.
	for offset := 0; offset <= len(b); offset += runeSizeAt(b, offset) {
		pos, ok := UTF16PositionForByteOffset(b, offset)
		if !ok {
			t.Fatalf("UTF16PositionForByteOffset(%d): not ok", offset)
		}
		back, ok := byteOffsetForUTF16Position(b, pos)
		if !ok {
			t.Fatalf("offset %d -> pos %+v -> back: not ok", offset, pos)
		}
		if back != offset {
			t.Fatalf("offset %d -> pos %+v -> back %d, want %d", offset, pos, back, offset)
		}
	}
}

// runeSizeAt returns the byte width of the rune starting at offset, or 1 to
// terminate the loop at the very end of b.
func runeSizeAt(b []byte, offset int) int {
	if offset >= len(b) {
		return 1
	}
	_, size := utf8.DecodeRune(b[offset:])
	return size
}

// TestUTF16PositionConverter_MatchesUTF16PositionForByteOffset pins
// UTF16PositionConverter's incremental result to the non-incremental
// UTF16PositionForByteOffset it is a faster substitute for, both for a
// realistic ascending sequence of offsets (its intended use: converting
// hints already sorted by offset) and for an out-of-order offset (which
// must fall back to rescanning from the start rather than returning a
// wrong position computed from the wrong cursor).
func TestUTF16PositionConverter_MatchesUTF16PositionForByteOffset(t *testing.T) {
	text := []byte("package main\n\nfunc main() {\n\tprintln(\"héllo, 世界 \U0001F600\")\n}\n")

	t.Run("ascending offsets", func(t *testing.T) {
		conv := NewUTF16PositionConverter(text)
		for offset := 0; offset <= len(text); offset += runeSizeAt(text, offset) {
			want, wantOK := UTF16PositionForByteOffset(text, offset)
			got, gotOK := conv.Position(offset)
			if gotOK != wantOK || got != want {
				t.Fatalf("Position(%d) = %+v, %v; want %+v, %v", offset, got, gotOK, want, wantOK)
			}
		}
	})

	t.Run("out of order offset falls back correctly", func(t *testing.T) {
		conv := NewUTF16PositionConverter(text)
		if _, ok := conv.Position(40); !ok {
			t.Fatalf("Position(40): not ok")
		}
		got, gotOK := conv.Position(5) // earlier than the cursor's current position
		want, wantOK := UTF16PositionForByteOffset(text, 5)
		if gotOK != wantOK || got != want {
			t.Fatalf("Position(5) after Position(40) = %+v, %v; want %+v, %v", got, gotOK, want, wantOK)
		}
	})

	t.Run("offset outside text is rejected", func(t *testing.T) {
		conv := NewUTF16PositionConverter(text)
		if _, ok := conv.Position(len(text) + 1); ok {
			t.Fatalf("Position(len+1): ok, want not ok")
		}
		if _, ok := conv.Position(-1); ok {
			t.Fatalf("Position(-1): ok, want not ok")
		}
	})
}

func TestApplyContentChanges(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		changes []protocol.TextDocumentContentChangeEvent
		want    string
		wantErr bool
	}{
		{
			name: "whole document replace",
			text: "old",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangeWholeDocument{Text: "new"},
			},
			want: "new",
		},
		{
			name: "single incremental insert",
			text: "hello world",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{
						Start: protocol.Position{Line: 0, Character: 5},
						End:   protocol.Position{Line: 0, Character: 5},
					},
					Text: ",",
				},
			},
			want: "hello, world",
		},
		{
			name: "single incremental delete",
			text: "hello world",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{
						Start: protocol.Position{Line: 0, Character: 5},
						End:   protocol.Position{Line: 0, Character: 11},
					},
					Text: "",
				},
			},
			want: "hello",
		},
		{
			name: "multi-line range replace",
			text: "line1\nline2\nline3",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{
						Start: protocol.Position{Line: 0, Character: 5},
						End:   protocol.Position{Line: 2, Character: 0},
					},
					Text: " ",
				},
			},
			want: "line1 line3",
		},
		{
			name: "sequential changes compose left to right",
			text: "abc",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 0}},
					Text:  "X",
				},
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 4}, End: protocol.Position{Line: 0, Character: 4}},
					Text:  "Y",
				},
			},
			want: "Xabc" + "Y", // second change's positions are relative to the state after the first
		},
		{
			name: "insert astral character mid line",
			text: "ab",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 1}, End: protocol.Position{Line: 0, Character: 1}},
					Text:  "\U0001F600",
				},
			},
			want: "a\U0001F600b",
		},
		{
			name: "invalid range start",
			text: "abc",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{Start: protocol.Position{Line: 9, Character: 0}, End: protocol.Position{Line: 9, Character: 0}},
					Text:  "x",
				},
			},
			wantErr: true,
		},
		{
			name: "end before start",
			text: "abcdef",
			changes: []protocol.TextDocumentContentChangeEvent{
				&protocol.TextDocumentContentChangePartial{
					Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 4}, End: protocol.Position{Line: 0, Character: 1}},
					Text:  "x",
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyContentChanges([]byte(tt.text), tt.changes)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("applyContentChanges() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("applyContentChanges() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("applyContentChanges() = %q, want %q", got, tt.want)
			}
		})
	}
}
