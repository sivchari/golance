package langfeat_test

// This file closes audit-informational.md's "UNVERIFIABLE this pass" gap
// for foldingRange: it drives a real gopls v0.23.0 (skipped if not on
// PATH) as the parity oracle for langfeat.FoldingRanges, against
// testdata/module/parityaudit/parityaudit.go — a fixture covering
// generics, value/pointer receiver methods, variadics, multi-return/named
// results, embedded types, interfaces, a const block, struct literals, and
// a long function body with nested blocks plus a multi-line comment.
//
// `gopls folding_ranges` folds a much richer set of syntax shapes than
// golance does (parameter lists, argument lists, composite literals,
// generic type-parameter lists, and more — see FoldingRanges's own doc:
// golance only folds block statements, struct/interface bodies, import
// blocks, and multi-line comment groups). That is a documented, narrower
// design scope, not a bug, so this is a containment check, not a set
// equality one: every (startLine, endLine) golance reports must also
// appear somewhere in gopls's own output, catching a genuinely wrong or
// invented range without failing on gopls's many additional fold points
// golance never claimed to support.

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// goplsLineSpan is one gopls "startLine:startCol-endLine:endCol" folding
// range, reduced to its 1-based line span (column is not part of this
// comparison: golance's own protocol.FoldingRange never sets
// StartCharacter/EndCharacter — see handlers_nav.go's handleFoldingRange —
// so only line-granularity parity is meaningful here).
type goplsLineSpan struct {
	startLine, endLine int
}

var goplsFoldingRangeRE = regexp.MustCompile(`^(\d+):\d+-(\d+):\d+$`)

// runGoplsFoldingRanges runs `gopls folding_ranges path` and parses its
// output into goplsLineSpan entries.
func runGoplsFoldingRanges(t *testing.T, path string) []goplsLineSpan {
	t.Helper()
	args := []string{"folding_ranges"}
	args = append(args, path)
	cmd := exec.Command("gopls", args...)
	cmd.Env = append(os.Environ(), "GOCACHE="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gopls folding_ranges %s: %v", path, err)
	}

	var spans []goplsLineSpan
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		m := goplsFoldingRangeRE.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unparseable gopls folding_ranges line %q", line)
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse start line in %q: %v", line, err)
		}
		end, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("parse end line in %q: %v", line, err)
		}
		spans = append(spans, goplsLineSpan{startLine: start, endLine: end})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan gopls folding_ranges output: %v", err)
	}
	return spans
}

// TestFoldingRanges_GoplsParity asserts every range langfeat.FoldingRanges
// reports for parityaudit.go also appears in gopls's own folding_ranges
// output at the same 1-based line span — see this file's own doc for why
// this is a containment, not an equality, check.
func TestFoldingRanges_GoplsParity(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "parityaudit", "parityaudit.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	got, err := langfeat.FoldingRanges(cp, path)
	if err != nil {
		t.Fatalf("FoldingRanges: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("FoldingRanges returned no ranges for a fixture with import/struct/comment/nested-block folds")
	}

	want := make(map[goplsLineSpan]bool)
	for _, s := range runGoplsFoldingRanges(t, path) {
		want[s] = true
	}

	for _, fr := range got {
		startLine, _ := lineCol(text, fr.Range.StartOffset)
		endLine, _ := lineCol(text, fr.Range.EndOffset)
		span := goplsLineSpan{startLine: startLine, endLine: endLine}
		if !want[span] {
			t.Errorf("FoldingRanges reports %v (kind %v) at line span %d-%d, which gopls does not fold at all", fr.Range, fr.Kind, startLine, endLine)
		}
	}
}
