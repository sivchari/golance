package server

import (
	"bytes"
	"math"
	"unicode/utf8"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/overlay"
)

// byteOffsetForPosition converts an LSP Position (UTF-16 line/character
// offsets, both 0-based) to a byte offset into text. It is the inverse of
// overlay.UTF16PositionForByteOffset — this package pairs the two, using
// overlay's exported function directly for the byte-offset-to-Position
// direction and this one for the reverse, since overlay's own reverse
// helper (byteOffsetForUTF16Position) is unexported. ok is false if pos
// names a line or character position past the end of text, or a character
// offset that lands inside a UTF-16 surrogate pair.
func byteOffsetForPosition(text []byte, pos protocol.Position) (int, bool) {
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

// utf16Units returns how many UTF-16 code units r encodes as: 2 for runes
// outside the Basic Multilingual Plane (which need a surrogate pair), 1
// otherwise. Mirrors overlay's unexported helper of the same name.
func utf16Units(r rune) uint32 {
	if r > 0xffff {
		return 2
	}
	return 1
}

// lineColForByteOffset converts a 0-based byte offset into text to a
// 1-based line and 1-based byte column: the coordinate system
// internal/xref and internal/store use (matching go/token.Position, whose
// doc says "Column is the column number, in bytes, starting at 1"). offset
// must be in [0, len(text)].
func lineColForByteOffset(text []byte, offset int) (line, col uint32) {
	line = 1
	lineStart := 0
	for i := 0; i < offset && i < len(text); i++ {
		if text[i] == '\n' {
			line++
			lineStart = i + 1
		}
	}
	// lineStart <= offset always holds here (the loop only ever sets
	// lineStart to i+1 for an i < offset), so this is never negative in
	// practice; guarded explicitly rather than relying on that invariant.
	d := offset - lineStart
	if d < 0 {
		d = 0
	}
	if d > math.MaxUint32 {
		d = math.MaxUint32
	}
	return line, uint32(d) + 1
}

// byteOffsetForLineCol converts a 1-based line number and 1-based byte
// column (the coordinate system internal/xref and internal/store use) to a
// 0-based byte offset into text: the inverse of lineColForByteOffset. ok is
// false if line or col is out of range for text.
func byteOffsetForLineCol(text []byte, line, col uint32) (int, bool) {
	if line == 0 || col == 0 {
		return 0, false
	}
	i := 0
	for l := uint32(1); l < line; l++ {
		nl := bytes.IndexByte(text[i:], '\n')
		if nl < 0 {
			return 0, false
		}
		i += nl + 1
	}
	lineEnd := len(text)
	if nl := bytes.IndexByte(text[i:], '\n'); nl >= 0 {
		lineEnd = i + nl
	}
	offset := i + int(col) - 1
	if offset < i || offset > lineEnd {
		return 0, false
	}
	return offset, true
}

// positionToXref converts an LSP Position (UTF-16, against text's current
// content) to the 1-based line/byte-column coordinates internal/xref
// queries take.
func positionToXref(text []byte, pos protocol.Position) (line, col int, ok bool) {
	offset, ok := byteOffsetForPosition(text, pos)
	if !ok {
		return 0, 0, false
	}
	l, c := lineColForByteOffset(text, offset)
	return int(l), int(c), true
}

// xrefRangeToLSP converts a single-line span given in internal/xref's
// 1-based line/byte-column coordinates ([col, endCol) on line) into an LSP
// Range against text's current content.
func xrefRangeToLSP(text []byte, line, col, endCol uint32) (protocol.Range, bool) {
	startOff, ok := byteOffsetForLineCol(text, line, col)
	if !ok {
		return protocol.Range{}, false
	}
	endOff, ok := byteOffsetForLineCol(text, line, endCol)
	if !ok {
		return protocol.Range{}, false
	}
	start, ok := overlay.UTF16PositionForByteOffset(text, startOff)
	if !ok {
		return protocol.Range{}, false
	}
	end, ok := overlay.UTF16PositionForByteOffset(text, endOff)
	if !ok {
		return protocol.Range{}, false
	}
	return protocol.Range{Start: start, End: end}, true
}

// offsetRangeToLSP converts a [start, end) byte-offset span (the coordinate
// system internal/langfeat uses) into an LSP Range against text's current
// content.
func offsetRangeToLSP(text []byte, start, end int) (protocol.Range, bool) {
	sp, ok := overlay.UTF16PositionForByteOffset(text, start)
	if !ok {
		return protocol.Range{}, false
	}
	ep, ok := overlay.UTF16PositionForByteOffset(text, end)
	if !ok {
		return protocol.Range{}, false
	}
	return protocol.Range{Start: sp, End: ep}, true
}

// buildLineStarts returns the byte offset where each of text's lines
// begins, indexed by (line number - 1): buildLineStarts(text)[0] == 0, and
// element i+1 is the byte right after text's i-th newline. Precomputing
// this once per file lets offsetForLineCol locate any line in O(1) instead
// of byteOffsetForLineCol's O(line) scan from byte 0 -- what lets
// toLSPLocations' per-file cache (xrefFileEntry) convert a references
// result in O(file size + results) rather than O(results x file size).
func buildLineStarts(text []byte) []int {
	starts := make([]int, 1, 64)
	for i, b := range text {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// offsetForLineCol is byteOffsetForLineCol's counterpart against a
// precomputed lineStarts table (see buildLineStarts) instead of text
// itself, with identical coordinates and validation. textLen must be
// len(the text lineStarts was built from).
func offsetForLineCol(lineStarts []int, textLen int, line, col uint32) (int, bool) {
	if line == 0 || col == 0 {
		return 0, false
	}
	idx := int(line) - 1
	if idx < 0 || idx >= len(lineStarts) {
		return 0, false
	}
	lineStart := lineStarts[idx]
	lineEnd := textLen
	if idx+1 < len(lineStarts) {
		lineEnd = lineStarts[idx+1] - 1
	}
	offset := lineStart + int(col) - 1
	if offset < lineStart || offset > lineEnd {
		return 0, false
	}
	return offset, true
}

// xrefFileEntry is toLSPLocations' per-file cache entry: text (read once,
// preferring dirtyLines' already-read overlay content over a second
// overlay.ReadFile call), a line-start table and UTF16PositionConverter
// built once from it, and the dirty-buffer line-correction inputs --
// everything correctResultRange/xrefRangeToLSP otherwise recompute from
// byte 0 on every single location. Mirrors chSourceFile's
// (handlers_callhierarchy.go) lifecycle: built fresh for one
// toLSPLocations call and discarded when it returns, never reused across
// requests, which is what makes caching a read failure as a nil entry
// below safe -- it only ever costs the one call it happened during.
type xrefFileEntry struct {
	text       []byte
	lineStarts []int
	conv       *overlay.UTF16PositionConverter
	saved      []byte
	dirty      []byte
	dirtyOK    bool
}

// xrefFileEntryFor returns file's cached xrefFileEntry from cache, building
// it on first use. A read failure caches a nil entry so every subsequent
// location in file is dropped without retrying the read, matching
// correctResultRange's existing per-location behavior for the same
// failure.
func (s *Server) xrefFileEntryFor(file string, cache map[string]*xrefFileEntry) *xrefFileEntry {
	if e, ok := cache[file]; ok {
		return e
	}
	saved, dirty, dirtyOK := s.dirtyLines(file)
	text := dirty
	if !dirtyOK {
		t, err := s.overlay.ReadFile(file)
		if err != nil {
			cache[file] = nil
			return nil
		}
		text = t
	}
	e := &xrefFileEntry{
		text:       text,
		lineStarts: buildLineStarts(text),
		conv:       overlay.NewUTF16PositionConverter(text),
		saved:      saved,
		dirty:      dirty,
		dirtyOK:    dirtyOK,
	}
	cache[file] = e
	return e
}

// rangeFor converts a facts-index span (line, [col, endCol) 1-based byte
// columns) on e's file into an LSP Range, applying e's dirty-buffer line
// correction first -- e's own cached counterpart to
// correctResultRange/xrefRangeToLSP.
func (e *xrefFileEntry) rangeFor(line, col, endCol uint32) (protocol.Range, bool) {
	if e.dirtyOK {
		if mapped, ok := dirtyLineMap(e.saved, e.dirty, line); ok {
			line = mapped
		}
	}
	startOff, ok := offsetForLineCol(e.lineStarts, len(e.text), line, col)
	if !ok {
		return protocol.Range{}, false
	}
	endOff, ok := offsetForLineCol(e.lineStarts, len(e.text), line, endCol)
	if !ok {
		return protocol.Range{}, false
	}
	start, ok := e.conv.Position(startOff)
	if !ok {
		return protocol.Range{}, false
	}
	end, ok := e.conv.Position(endOff)
	if !ok {
		return protocol.Range{}, false
	}
	return protocol.Range{Start: start, End: end}, true
}
