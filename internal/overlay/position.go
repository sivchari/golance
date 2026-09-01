package overlay

import (
	"fmt"
	"unicode/utf8"

	"go.lsp.dev/protocol"
)

// applyContentChanges applies a didChange notification's content changes to
// text in order, per the LSP contract that change i is computed against the
// document state produced by change i-1.
func applyContentChanges(text []byte, changes []protocol.TextDocumentContentChangeEvent) ([]byte, error) {
	for _, ch := range changes {
		switch c := ch.(type) {
		case *protocol.TextDocumentContentChangeWholeDocument:
			text = []byte(c.Text)
		case *protocol.TextDocumentContentChangePartial:
			start, ok := byteOffsetForUTF16Position(text, c.Range.Start)
			if !ok {
				return nil, fmt.Errorf("overlay: invalid range start %+v", c.Range.Start)
			}
			end, ok := byteOffsetForUTF16Position(text, c.Range.End)
			if !ok || end < start {
				return nil, fmt.Errorf("overlay: invalid range end %+v", c.Range.End)
			}
			merged := make([]byte, 0, len(text)-(end-start)+len(c.Text))
			merged = append(merged, text[:start]...)
			merged = append(merged, c.Text...)
			merged = append(merged, text[end:]...)
			text = merged
		default:
			return nil, fmt.Errorf("overlay: unsupported content change type %T", ch)
		}
	}
	return text, nil
}

// byteOffsetForUTF16Position converts an LSP Position (UTF-16 line/character
// offsets) to a byte offset into text. It returns false if pos names a line
// or character position past the end of text or lands inside a UTF-16
// surrogate pair.
func byteOffsetForUTF16Position(text []byte, pos protocol.Position) (int, bool) {
	i := 0
	for line := uint32(0); line < pos.Line; {
		if i >= len(text) {
			return 0, false
		}
		r, size := utf8.DecodeRune(text[i:])
		i += size
		if r == '\n' {
			line++
		}
	}
	col := uint32(0)
	for i < len(text) {
		r, size := utf8.DecodeRune(text[i:])
		if r == '\n' {
			break
		}
		if col == pos.Character {
			return i, true
		}
		col += utf16Units(r)
		if col > pos.Character {
			return 0, false
		}
		i += size
	}
	if col == pos.Character {
		return i, true
	}
	return 0, false
}

// UTF16PositionForByteOffset converts a byte offset into text to an LSP
// Position (UTF-16 line/character offsets), the inverse of
// byteOffsetForUTF16Position. Used to report diagnostics computed from
// go/token byte offsets in LSP coordinates.
func UTF16PositionForByteOffset(text []byte, offset int) (protocol.Position, bool) {
	if offset < 0 || offset > len(text) {
		return protocol.Position{}, false
	}
	line := uint32(0)
	lineStart := 0
	for i := 0; i < offset; {
		r, size := utf8.DecodeRune(text[i:])
		i += size
		if r == '\n' {
			line++
			lineStart = i
		}
	}
	col := uint32(0)
	for i := lineStart; i < offset; {
		r, size := utf8.DecodeRune(text[i:])
		col += utf16Units(r)
		i += size
	}
	return protocol.Position{Line: line, Character: col}, true
}

// UTF16PositionConverter converts a series of byte offsets into text to LSP
// UTF-16 Positions like UTF16PositionForByteOffset, but in amortized O(1)
// per call (O(n) total across the whole series) when the offsets are
// non-decreasing, by resuming the scan from wherever the previous call left
// off instead of rescanning text from byte 0 every time. A caller
// converting many offsets already sorted ascending — e.g. one per inlay
// hint, as langfeat.InlayHints returns them — should use this instead of
// calling UTF16PositionForByteOffset in a loop, which costs O(n*m) for n
// offsets across an m-byte file. An offset lower than the previous one is
// still handled correctly, by falling back to a rescan from the start.
type UTF16PositionConverter struct {
	text []byte
	pos  int
	line uint32
	col  uint32
}

// NewUTF16PositionConverter returns a converter for text, positioned at the
// start of the file.
func NewUTF16PositionConverter(text []byte) *UTF16PositionConverter {
	return &UTF16PositionConverter{text: text}
}

// Position converts offset to its UTF-16 line/character Position, advancing
// the converter's cursor to offset. It returns false for an offset outside
// [0, len(text)].
func (c *UTF16PositionConverter) Position(offset int) (protocol.Position, bool) {
	if offset < 0 || offset > len(c.text) {
		return protocol.Position{}, false
	}
	if offset < c.pos {
		c.pos, c.line, c.col = 0, 0, 0
	}
	for c.pos < offset {
		r, size := utf8.DecodeRune(c.text[c.pos:])
		c.pos += size
		if r == '\n' {
			c.line++
			c.col = 0
		} else {
			c.col += utf16Units(r)
		}
	}
	return protocol.Position{Line: c.line, Character: c.col}, true
}

func utf16Units(r rune) uint32 {
	if r > 0xffff {
		return 2
	}
	return 1
}
