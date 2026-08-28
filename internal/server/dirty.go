package server

import (
	"bytes"
	"crypto/sha256"
	"math"
	"os"
	"path/filepath"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// dirtyLineMap maps a 1-based line number in from's content to the
// corresponding 1-based line number in to's content, using a naive
// top-down line diff: every line above the first point of divergence is
// unchanged, and every line at or below it shifts by the constant
// line-count delta between from and to (plan-feat-v0.1.md, "4. incremental
// reindex", step 4: a plain line-count-shift position correction). This is
// not a real diff — edits in different parts of the same file produce one
// boundary at the first divergence found from the top, not a minimal edit
// script. ok is false if the mapped line falls outside to's range, in
// which case the caller should use line uncorrected.
func dirtyLineMap(from, to []byte, line uint32) (uint32, bool) {
	fromLines := bytes.Split(from, []byte{'\n'})
	toLines := bytes.Split(to, []byte{'\n'})

	boundary := 0
	for boundary < len(fromLines) && boundary < len(toLines) && bytes.Equal(fromLines[boundary], toLines[boundary]) {
		boundary++
	}

	idx := int(line) - 1 // 0-based
	if idx < 0 {
		return 0, false
	}
	if idx < boundary {
		return line, true
	}

	mapped := idx + (len(toLines) - len(fromLines))
	return mappedLine(mapped, boundary, len(toLines))
}

// mappedLine converts mapped to a 1-based uint32 line number, if it falls
// within [boundary, toLineCount) — a plain sequence of early returns (no
// compound conditions) so each one is independently provable.
func mappedLine(mapped, boundary, toLineCount int) (uint32, bool) {
	if mapped < boundary {
		return 0, false
	}
	if mapped >= toLineCount {
		return 0, false
	}
	if mapped < 0 {
		return 0, false
	}
	if mapped > math.MaxUint32 {
		return 0, false
	}
	return uint32(mapped) + 1, true
}

// diskReadFile reads path's on-disk content directly, bypassing the
// overlay. Documents in an LSP session are always addressed by a
// client-provided URI naming a file the client itself is editing, the same
// trust boundary os.ReadFile-based file reading elsewhere in this server
// (and in overlay.Overlay.ReadFile) already relies on.
func diskReadFile(path string) ([]byte, error) {
	return os.ReadFile(filepath.Clean(path))
}

// dirtyLines returns path's on-disk (saved) and editor-buffer (overlay)
// content if path is currently open with unsaved changes. ok is false if
// path is not open, its saved content cannot be read, or its saved and
// overlay content are identical — callers should skip line correction in
// every one of those cases.
func (s *Server) dirtyLines(path string) (saved, dirty []byte, ok bool) {
	text, _, hash, open := s.overlay.Get(uri.File(path))
	if !open {
		return nil, nil, false
	}
	disk, err := diskReadFile(path)
	if err != nil {
		return nil, nil, false
	}
	if sha256.Sum256(disk) == hash {
		return nil, nil, false
	}
	return disk, text, true
}

// correctQueryLine maps line — a 1-based line number against path's
// current editor buffer — back to the on-disk line the facts index was
// built from, if path has unsaved changes. It returns line unchanged if
// path is clean or the shift cannot be determined.
func (s *Server) correctQueryLine(path string, line uint32) uint32 {
	saved, dirty, ok := s.dirtyLines(path)
	if !ok {
		return line
	}
	if mapped, ok := dirtyLineMap(dirty, saved, line); ok {
		return mapped
	}
	return line
}

// correctResultRange converts a facts-index (on-disk) span in file — a
// 1-based line and a [col, endCol) 1-based byte-column range on it — into
// an LSP Range against file's current editor-buffer content, first
// correcting the line number for unsaved edits if file has any.
func (s *Server) correctResultRange(file string, line, col, endCol uint32) (protocol.Range, bool) {
	if saved, dirty, ok := s.dirtyLines(file); ok {
		if mapped, ok := dirtyLineMap(saved, dirty, line); ok {
			line = mapped
		}
	}
	text, err := s.overlay.ReadFile(file)
	if err != nil {
		return protocol.Range{}, false
	}
	return xrefRangeToLSP(text, line, col, endCol)
}
