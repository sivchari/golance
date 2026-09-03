// Package diff computes the byte-range edits needed to turn one text into
// another, at line granularity, using Myers' shortest-edit-script
// algorithm (E. Myers, "An O(ND) Difference Algorithm and Its Variations",
// 1986) — the same algorithm gopls's own internal/diff package uses for
// textDocument/formatting (gopls@v0.23.0's internal/golang/format.go,
// computeTextEdits -> internal/diff.Strings -> internal/diff/lcs). That
// package is unimportable here (golang.org/x/tools/internal/diff/lcs is
// internal to the golang.org/x/tools module, and golance is a separate
// module), so this reimplements the algorithm at line granularity — a
// clean line-level diff, rather than gopls's finer rune-level one, which
// is sufficient for gofmt/goimports output (every change gofmt/goimports
// makes replaces whole lines) and already matches what an editor expects
// from a minimal formatting edit.
package diff

import "strings"

// Edit describes replacing the half-open byte range [Start, End) of the
// "before" text passed to Lines with New. Start and End always fall on a
// line boundary in "before" — the start of a line or the end of the file.
type Edit struct {
	Start, End int
	New        string
}

// Lines returns the edits needed to turn before into after, computed at
// line granularity (each line carries its own trailing '\n', so every
// edit's Start and End land exactly on a line boundary) via Myers'
// shortest-edit-script algorithm. It returns nil if before and after split
// into identical lines.
func Lines(before, after []byte) []Edit {
	beforeLines, beforeOffsets := splitLines(before)
	afterLines, _ := splitLines(after)
	ops := diffLines(beforeLines, afterLines)
	return editsFromOps(ops, afterLines, beforeOffsets)
}

// splitLines splits text into lines, each retaining its own trailing '\n'
// (the final line has none if text does not itself end in one), and
// returns the byte offset at which each line begins. offsets has one more
// entry than lines: offsets[len(lines)] is len(text), letting
// editsFromOps address "the very end of the file" the same way it
// addresses any other line boundary.
func splitLines(text []byte) (lines []string, offsets []int) {
	offsets = append(offsets, 0)
	start := 0
	for i, b := range text {
		if b == '\n' {
			lines = append(lines, string(text[start:i+1]))
			start = i + 1
			offsets = append(offsets, start)
		}
	}
	if start < len(text) {
		lines = append(lines, string(text[start:]))
		offsets = append(offsets, len(text))
	}
	return lines, offsets
}

// opKind categorizes one step of a shortest edit script.
type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

// op is one step of a shortest edit script, in document order: an equal
// or delete step references its line in "before" (aIdx); an equal or
// insert step references its line in "after" (bIdx).
type op struct {
	kind opKind
	aIdx int
	bIdx int
}

// diffLines returns the shortest edit script transforming a into b, as a
// sequence of equal/delete/insert ops in document order.
func diffLines(a, b []string) []op {
	trace, offset := shortestEditTrace(a, b)
	return backtrack(len(a), len(b), trace, offset)
}

// shortestEditTrace runs Myers' greedy O(ND) algorithm and returns the
// sequence of V-array snapshots backtrack needs to reconstruct the
// shortest edit script: trace[d] is V as it stood immediately before d's
// own diagonals were computed. offset centers each V array so k (which
// ranges over [-d, d]) maps to index offset+k, avoiding negative slice
// indices.
func shortestEditTrace(a, b []string) (trace [][]int, offset int) {
	n, m := len(a), len(b)
	maxD := n + m
	if maxD == 0 {
		return nil, 0
	}
	offset = maxD
	v := make([]int, 2*maxD+1)
	for d := 0; d <= maxD; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return trace, offset
			}
		}
	}
	return trace, offset // unreachable: d == n+m always finds x>=n && y>=m
}

// backtrack walks trace from the end (a and b both fully consumed) back to
// the start, recovering the shortest edit script in reverse, then reverses
// it into document order.
func backtrack(n, m int, trace [][]int, offset int) []op {
	x, y := n, m
	var ops []op
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK

		for x > prevX && y > prevY {
			ops = append(ops, op{kind: opEqual, aIdx: x - 1, bIdx: y - 1})
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				ops = append(ops, op{kind: opInsert, bIdx: y - 1})
			} else {
				ops = append(ops, op{kind: opDelete, aIdx: x - 1})
			}
			x, y = prevX, prevY
		}
	}
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}

// editsFromOps converts ops (in document order) into the minimal set of
// byte-range Edits against "before"'s own text: each maximal run of
// consecutive delete/insert ops (bounded by equal ops, or by the start/end
// of the file) becomes a single Edit, using beforeOffsets to convert
// before-line indices into byte offsets and afterLines to fetch inserted
// lines' text.
func editsFromOps(ops []op, afterLines []string, beforeOffsets []int) []Edit {
	var edits []Edit
	var pending *Edit
	var pendingLines []string
	lastAEnd := 0 // before-line index just past the most recently seen equal op

	flush := func() {
		if pending == nil {
			return
		}
		pending.New = strings.Join(pendingLines, "")
		edits = append(edits, *pending)
		pending = nil
		pendingLines = nil
	}

	for _, o := range ops {
		switch o.kind {
		case opEqual:
			flush()
			lastAEnd = o.aIdx + 1
		case opDelete:
			if pending == nil {
				pending = &Edit{Start: beforeOffsets[o.aIdx], End: beforeOffsets[o.aIdx+1]}
			} else {
				pending.End = beforeOffsets[o.aIdx+1]
			}
		case opInsert:
			if pending == nil {
				pending = &Edit{Start: beforeOffsets[lastAEnd], End: beforeOffsets[lastAEnd]}
			}
			pendingLines = append(pendingLines, afterLines[o.bIdx])
		}
	}
	flush()
	return edits
}
