package check

import (
	"go/token"
	"unicode"
	"unicode/utf8"

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
		if d, ok := diagAt(reader, e.Pos, e.Msg, SeverityError); ok {
			out = append(out, d)
		}
	}
	for _, e := range cp.typeErrs {
		pos := cp.fset.Position(e.Pos)
		sev := SeverityError
		if e.Soft {
			sev = SeverityWarning
		}
		if d, ok := diagAt(reader, pos, e.Msg, sev); ok {
			out = append(out, d)
		}
	}
	return out
}

// diagAt builds a Diag at pos, extending the end position to the end of the
// identifier starting at pos when there is one, otherwise leaving start and
// end equal.
func diagAt(reader overlay.FileReader, pos token.Position, msg string, sev Severity) (Diag, bool) {
	text, err := reader.ReadFile(pos.Filename)
	if err != nil {
		return Diag{}, false
	}
	start, ok := overlay.UTF16PositionForByteOffset(text, pos.Offset)
	if !ok {
		return Diag{}, false
	}
	end := start
	if endOffset := identEnd(text, pos.Offset); endOffset > pos.Offset {
		if e, ok := overlay.UTF16PositionForByteOffset(text, endOffset); ok {
			end = e
		}
	}
	return Diag{
		File:      pos.Filename,
		StartLine: start.Line,
		StartCol:  start.Character,
		EndLine:   end.Line,
		EndCol:    end.Character,
		Message:   msg,
		Severity:  sev,
	}, true
}

// identEnd returns the byte offset one past the identifier starting at
// offset in text, or offset itself if text[offset:] does not start with an
// identifier.
func identEnd(text []byte, offset int) int {
	i := offset
	first := true
	for i < len(text) {
		r, size := utf8.DecodeRune(text[i:])
		if !isIdentRune(r, first) {
			break
		}
		i += size
		first = false
	}
	return i
}

func isIdentRune(r rune, first bool) bool {
	if first {
		return unicode.IsLetter(r) || r == '_'
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
