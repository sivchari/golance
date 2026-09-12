package langfeat_test

// This closes audit-informational.md's rangeFormatting entry in the
// "UNVERIFIABLE this pass" finding: gopls has no CLI subcommand for
// rangeFormatting, and textDocument/rangeFormatting (handlers_nav.go's
// handleDocumentRangeFormatting) formats through langfeat.Format alone
// (never langfeat.OrganizeImports' goimports-derived import management),
// so the feature's correctness reduces to two invariants of Format itself:
// it is exactly go/format.Source (the same formatter gopls's own
// rangeFormatting/formatting handlers call), and it is idempotent.

import (
	"bytes"
	"go/format"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
)

const formatTestUnformattedSrc = `package p

func Add(a,b int) int {
	return   a+b
}
`

// TestFormat_MatchesGoFormatSource asserts langfeat.Format's output is
// byte-for-byte identical to go/format.Source's own — the same formatter
// gopls itself uses — for both an already-formatted and an unformatted
// input.
func TestFormat_MatchesGoFormatSource(t *testing.T) {
	for name, src := range map[string]string{
		"AlreadyFormatted": "package p\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"Unformatted":      formatTestUnformattedSrc,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := langfeat.Format([]byte(src))
			if err != nil {
				t.Fatalf("Format: %v", err)
			}
			want, err := format.Source([]byte(src))
			if err != nil {
				t.Fatalf("format.Source: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Format(%q) = %q, want %q (go/format.Source)", src, got, want)
			}
		})
	}
}

// TestFormat_Idempotent asserts formatting already-formatted output
// changes nothing — a second textDocument/rangeFormatting (or formatting)
// request against a file the first one already fixed must report no
// further edits.
func TestFormat_Idempotent(t *testing.T) {
	once, err := langfeat.Format([]byte(formatTestUnformattedSrc))
	if err != nil {
		t.Fatalf("Format (first pass): %v", err)
	}
	twice, err := langfeat.Format(once)
	if err != nil {
		t.Fatalf("Format (second pass): %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("Format is not idempotent: first pass = %q, second pass = %q", once, twice)
	}
}
