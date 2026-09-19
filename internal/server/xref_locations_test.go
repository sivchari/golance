package server

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/xref"
)

// noTrailingNewlineText exercises a last line that ends exactly at EOF with
// no trailing newline byte, a boundary buildLineStarts/offsetForLineCol
// must handle the same way byteOffsetForLineCol already does.
const noTrailingNewlineText = "package main\n\nfunc main() {}"

// equivalenceFixtures are the texts TestOffsetForLineCol_MatchesByteOffsetForLineCol
// and TestXrefFileEntryRangeFor_MatchesXrefRangeToLSP sweep: plain ASCII,
// multi-byte (Japanese) and astral (emoji surrogate pair) UTF-8, a file with
// no trailing newline, and an empty file.
var equivalenceFixtures = map[string]string{
	"ascii":               asciiText,
	"japanese":            jpText,
	"emoji":               emojiText,
	"no trailing newline": noTrailingNewlineText,
	"empty":               "",
}

// sweepBound converts a fixture length into the sweep's inclusive upper
// bound, two past the last valid value so each sweep also covers the
// out-of-range side. The bound check is the in-range edge gosec's G115
// needs to see for the int -> uint32 narrowing.
func sweepBound(t *testing.T, n int) uint32 {
	t.Helper()
	bound := n + 2
	if bound >= 2 && bound <= math.MaxUint32 {
		return uint32(bound)
	}
	t.Fatalf("fixture length %d out of sweep range", n)
	return 0
}

// TestOffsetForLineCol_MatchesByteOffsetForLineCol checks, for every (line,
// col) pair in and around each fixture's bounds, that offsetForLineCol
// against a precomputed lineStarts table returns exactly what
// byteOffsetForLineCol (the oracle: scans text from byte 0) returns.
func TestOffsetForLineCol_MatchesByteOffsetForLineCol(t *testing.T) {
	for name, text := range equivalenceFixtures {
		t.Run(name, func(t *testing.T) {
			b := []byte(text)
			lineStarts := buildLineStarts(b)
			maxLine := sweepBound(t, len(lineStarts))
			maxCol := sweepBound(t, len(b))
			for line := uint32(0); line <= maxLine; line++ {
				for col := uint32(0); col <= maxCol; col++ {
					want, wantOK := byteOffsetForLineCol(b, line, col)
					got, ok := offsetForLineCol(lineStarts, len(b), line, col)
					if ok != wantOK || (ok && got != want) {
						t.Fatalf("offsetForLineCol(%d,%d) = (%d,%v), want (%d,%v)", line, col, got, ok, want, wantOK)
					}
				}
			}
		})
	}
}

// TestXrefFileEntryRangeFor_MatchesXrefRangeToLSP checks, for every (line,
// col, endCol) combination in and around each fixture's bounds, that a
// clean (no dirty-buffer correction) xrefFileEntry's rangeFor returns
// exactly what xrefRangeToLSP (the oracle it replaces per-location) returns.
func TestXrefFileEntryRangeFor_MatchesXrefRangeToLSP(t *testing.T) {
	for name, text := range equivalenceFixtures {
		t.Run(name, func(t *testing.T) {
			b := []byte(text)
			e := &xrefFileEntry{
				text:       b,
				lineStarts: buildLineStarts(b),
				conv:       overlay.NewUTF16PositionConverter(b),
			}
			maxLine := sweepBound(t, len(e.lineStarts))
			maxCol := sweepBound(t, len(b))
			for line := uint32(0); line <= maxLine; line++ {
				for col := uint32(0); col <= maxCol; col++ {
					for endCol := col; endCol <= maxCol; endCol++ {
						want, wantOK := xrefRangeToLSP(b, line, col, endCol)
						got, ok := e.rangeFor(line, col, endCol)
						if ok != wantOK || (ok && got != want) {
							t.Fatalf("rangeFor(%d,%d,%d) = (%+v,%v), want (%+v,%v)", line, col, endCol, got, ok, want, wantOK)
						}
					}
				}
			}
		})
	}
}

// TestToLSPLocations_MatchesCorrectResultLocationOracle checks that
// toLSPLocations' per-file cache produces exactly the same result, in the
// same order, as calling the untouched single-shot correctResultLocation
// once per location -- across a closed disk-only file, an open clean file,
// an open dirty file (line correction must still apply), and a file that
// cannot be read at all (its locations must be dropped).
func TestToLSPLocations_MatchesCorrectResultLocationOracle(t *testing.T) {
	dir := t.TempDir()
	s := &Server{overlay: newTestOverlay()}

	pathA := writeTempFile(t, dir, "a.go", asciiText) // closed, disk-only

	pathC := writeTempFile(t, dir, "c.go", asciiText)
	openDoc(t, s, pathC, asciiText) // open, clean

	savedB := "package p\n\nfunc A() {}\n\nfunc B() {}\n"
	dirtyB := "package p\n\nfunc A() {}\n\n// new comment\n// another\nfunc B() {}\n"
	pathB := writeTempFile(t, dir, "b.go", savedB)
	openDoc(t, s, pathB, savedB)
	changeDoc(t, s, pathB, 2, dirtyB) // open, dirty: two lines inserted before "func B() {}"

	pathMissing := filepath.Join(dir, "missing.go") // never created, never opened

	locs := []xref.Location{
		{File: pathA, Line: 3, Col: 6, EndCol: 10},      // "main"
		{File: pathB, Line: 5, Col: 1, EndCol: 5},       // "func" of B, below the edit -> line shifts
		{File: pathC, Line: 1, Col: 1, EndCol: 8},       // "package"
		{File: pathMissing, Line: 1, Col: 1, EndCol: 2}, // unreadable -> dropped
		{File: pathB, Line: 1, Col: 1, EndCol: 8},       // "package", above the edit -> unchanged
		{File: pathA, Line: 99, Col: 1, EndCol: 2},      // out of range -> dropped
		{File: pathC, Line: 3, Col: 6, EndCol: 10},
	}

	var want protocol.LocationSlice
	for _, loc := range locs {
		if pl, ok := s.correctResultLocation(loc); ok {
			want = append(want, pl)
		}
	}

	got := s.toLSPLocations(context.Background(), locs)
	if len(got) != len(want) {
		t.Fatalf("toLSPLocations returned %d locations, want %d: got=%+v want=%+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("toLSPLocations[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestToLSPLocations_CanceledContextReturnsNil checks that a ctx already
// canceled before conversion starts makes toLSPLocations return nil
// immediately, rather than converting every location first -- see
// toLSPLocations' doc for why an already-canceled request must not run a
// (potentially large) conversion to completion.
func TestToLSPLocations_CanceledContextReturnsNil(t *testing.T) {
	dir := t.TempDir()
	s := &Server{overlay: newTestOverlay()}
	path := writeTempFile(t, dir, "a.go", asciiText)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	locs := []xref.Location{{File: path, Line: 3, Col: 6, EndCol: 10}}
	if got := s.toLSPLocations(ctx, locs); got != nil {
		t.Fatalf("toLSPLocations with a canceled ctx = %+v, want nil", got)
	}
}
