package xref

import (
	"context"
	"testing"
)

// TestImplementation_ReusesDecodedUnitsAcrossCandidates pins the perf fix
// for the "Go to Implementation" slow-query report: unitBlob used to
// re-read and re-decode every candidate's CAS blob on every single call,
// even for the SAME package within one query (e.g. a candidate type and its
// own method, both resolved from the same package's facts) and across
// repeated identical queries. This verifies both:
//
//  1. A single query already produces cache hits (candidates sharing a
//     package, or a method resolved from the same unit as its type).
//  2. A repeat of the exact same query causes zero new cache misses: every
//     package it needs was already decoded and cached by the first query.
func TestImplementation_ReusesDecodedUnitsAcrossCandidates(t *testing.T) {
	root := generateBenchModule(t, 12)
	r, snap := newBenchResolver(t, root)
	targetFile := benchTargetFile(t, snap)
	line, col := benchIdentPos(t, targetFile, "Iface")
	ctx := context.Background()

	locs, err := r.Implementation(ctx, targetFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (first query): %v", err)
	}
	if len(locs) == 0 {
		t.Fatalf("Implementation (first query) returned no results")
	}
	hitsAfterFirst, missesAfterFirst := r.units.stats()
	if hitsAfterFirst == 0 {
		t.Errorf("hits after first query = 0, want > 0 (candidates/methods sharing a package should already reuse a decode within one query)")
	}
	if missesAfterFirst == 0 {
		t.Errorf("misses after first query = 0, want > 0 (nothing was decoded before this query ran)")
	}

	locs, err = r.Implementation(ctx, targetFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (repeat query): %v", err)
	}
	if len(locs) == 0 {
		t.Fatalf("Implementation (repeat query) returned no results")
	}
	hitsAfterSecond, missesAfterSecond := r.units.stats()
	if missesAfterSecond != missesAfterFirst {
		t.Errorf("repeat query added %d cache misses, want 0 (every package it needs was already decoded by the first query)", missesAfterSecond-missesAfterFirst)
	}
	if hitsAfterSecond <= hitsAfterFirst {
		t.Errorf("repeat query recorded no new cache hits (hits stayed at %d)", hitsAfterFirst)
	}
}
