package check

import (
	"go/scanner"
	"go/token"
	"go/types"
	"reflect"

	"github.com/sivchari/golance/internal/overlay"
)

// Severity is a diagnostic's severity, independent of any LSP protocol
// type: converting to protocol.DiagnosticSeverity is the langfeat layer's
// job.
type Severity int

// Severity levels a Diag can have.
const (
	SeverityError Severity = iota
	SeverityWarning
)

// Diag is one parse or type-checking diagnostic, with positions already
// converted to UTF-16 (0-origin), the coordinate system LSP uses.
type Diag struct {
	File      string
	StartLine uint32
	StartCol  uint32
	EndLine   uint32
	EndCol    uint32
	Message   string
	Severity  Severity
}

// Diagnostics converts cp's parse and type errors into Diags, reading
// through reader (overlay-aware) to resolve byte offsets to UTF-16
// positions. A position that cannot be resolved (e.g. its file is no longer
// readable) is dropped rather than reported with a wrong location.
func Diagnostics(cp *CheckedPackage, reader overlay.FileReader) []Diag {
	var out []Diag
	for _, e := range cp.parseErrs {
		if d, ok := diagAt(reader, e.Pos, e.Pos, e.Msg, SeverityError); ok {
			out = append(out, d)
		}
	}
	for _, e := range cp.typeErrs {
		start, end, ok := typeErrorRange(cp.fset, e)
		if !ok {
			start = cp.fset.Position(e.Pos)
			end = start
		}
		sev := SeverityError
		if e.Soft {
			sev = SeverityWarning
		}
		if d, ok := diagAt(reader, start, end, e.Msg, sev); ok {
			out = append(out, d)
		}
	}
	return out
}

// diagAt builds a Diag spanning [start, end). When start and end are equal —
// a parse error (which carries only one position), or a type error whose own
// span collapsed to zero-width (see typeErrorRange) — end is extended to the
// end of the token starting at start, so the diagnostic is never zero-width
// when the underlying source has an actual token there.
func diagAt(reader overlay.FileReader, start, end token.Position, msg string, sev Severity) (Diag, bool) {
	text, err := reader.ReadFile(start.Filename)
	if err != nil {
		return Diag{}, false
	}
	startPos, ok := overlay.UTF16PositionForByteOffset(text, start.Offset)
	if !ok {
		return Diag{}, false
	}
	endOffset := end.Offset
	if endOffset <= start.Offset {
		endOffset = identEnd(text, start.Offset)
	}
	endPos := startPos
	if endOffset > start.Offset {
		if e, ok := overlay.UTF16PositionForByteOffset(text, endOffset); ok {
			endPos = e
		}
	}
	return Diag{
		File:      start.Filename,
		StartLine: startPos.Line,
		StartCol:  startPos.Character,
		EndLine:   endPos.Line,
		EndCol:    endPos.Character,
		Message:   msg,
		Severity:  sev,
	}, true
}

// typeErrorRange extracts the precise span err's own producer (go/types)
// computed for it — e.g. an entire CallExpr or CompositeLit, not just its
// start — via the unexported go116start/go116end fields every types.Error
// has carried since Go 1.16 (the same fields
// golang.org/x/tools/internal/typesinternal.ErrorCodeStartEnd reads,
// by the identical reflection technique, pending the still-open proposal
// https://go.dev/issue/71803 to make them part of the public API: this is
// what lets gopls itself report a type error's true extent instead of a
// single position). ok is false if the fields are unreadable (a future Go
// version could remove them) or the span collapsed to zero-width, in which
// case the caller falls back to a token-boundary heuristic.
func typeErrorRange(fset *token.FileSet, err types.Error) (start, end token.Position, ok bool) {
	v := reflect.ValueOf(err)
	startField := v.FieldByName("go116start")
	endField := v.FieldByName("go116end")
	if !startField.IsValid() || !endField.IsValid() {
		return token.Position{}, token.Position{}, false
	}
	startPos, endPos := token.Pos(startField.Int()), token.Pos(endField.Int())
	if !startPos.IsValid() || !endPos.IsValid() || startPos == endPos {
		return token.Position{}, token.Position{}, false
	}
	return fset.Position(startPos), fset.Position(endPos), true
}

// identEnd returns the byte offset one past the token starting at offset in
// text — an identifier, keyword, or literal (string, rune, number) — or
// offset itself if text[offset:] does not start with one of those.
func identEnd(text []byte, offset int) int {
	if offset < 0 || offset >= len(text) {
		return offset
	}
	fset := token.NewFileSet()
	f := fset.AddFile("", fset.Base(), len(text)-offset)
	var s scanner.Scanner
	s.Init(f, text[offset:], nil, 0)
	pos, tok, lit := s.Scan()
	if f.Offset(pos) != 0 {
		return offset
	}
	switch {
	case tok.IsLiteral(): // IDENT, INT, FLOAT, IMAG, CHAR, STRING
		return offset + len(lit)
	case tok.IsKeyword():
		return offset + len(tok.String())
	default:
		return offset
	}
}
