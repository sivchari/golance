package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// populatePostingsDB opens a fresh DB and writes numSrcs source packages'
// worth of postings, all referencing the SAME (targetPkgHash, targetIDHash)
// pair, locsPerSrc locations each — the "hot symbol" shape: one target
// referenced from every one of numSrcs packages, the reverse reference
// index's own worst case for a single PostingsFor call (see
// BenchmarkDBPostingsFor_HotSymbol's doc). Every OTHER source package in
// the batch also writes a handful of postings to distinct, never-queried
// targets, so the postings bucket's total size approximates a real
// workspace's (most symbols have few referencers; this benchmark's target
// is the outlier) rather than measuring an artificially tiny bucket.
func populatePostingsDB(b *testing.B, numSrcs, locsPerSrc int) (*DB, uint64, uint64) {
	b.Helper()
	const targetPkg, targetID = 1, 1

	db, err := Open(filepath.Join(b.TempDir(), "index.db"))
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	b.Cleanup(func() {
		if err := db.Close(); err != nil {
			b.Errorf("Close() error = %v", err)
		}
	})

	entries := make([]UnitEntry, numSrcs)
	for i := 0; i < numSrcs; i++ {
		src := uint64(i + 100) // never collides with targetPkg (1)
		postings := make([]PostingEntry, 0, locsPerSrc+1)
		for j := 0; j < locsPerSrc; j++ {
			postings = append(postings, PostingEntry{
				TargetPkgHash: targetPkg,
				TargetIDHash:  targetID,
				File:          fmt.Sprintf("pkg%d/file%d.go", i, j%8),
				Line:          uint32(j + 1),
				Col:           1,
				EndCol:        10,
			})
		}
		// A handful of unrelated postings, so this src's own manifest and
		// the bucket as a whole are not artificially tiny.
		postings = append(postings, PostingEntry{TargetPkgHash: src + 1_000_000, TargetIDHash: 1, File: "other.go", Line: 1, Col: 1, EndCol: 2})

		entries[i] = UnitEntry{
			PkgHash: src,
			Pointer: UnitPointer{BlobKey: src, ContentHash: src},
			Index:   PackageIndexEntries{Postings: postings},
		}
	}
	if err := db.PutUnitsBatch(entries); err != nil {
		b.Fatalf("PutUnitsBatch() error = %v", err)
	}
	return db, targetPkg, targetID
}

// BenchmarkDBPostingsFor_HotSymbol measures PostingsFor's own cost in
// isolation (no type-checking, no Resolver overhead — see
// internal/xref/references_closure_bench_test.go's
// BenchmarkReferences_ClosureScale for the end-to-end equivalent) against
// the reverse reference index's worst case: a single symbol referenced from
// every one of N source packages. Reported alongside b.N's own timing is
// wantResults (the query's actual result count) so a before/after
// comparison at increasing N demonstrates O(result size) scaling directly:
// cost per op should grow with N (more results), never with workspace size
// beyond N (there is no "rest of the workspace" for PostingsFor's own
// prefix scan to pay for, unlike a reverse-dependency closure walk over
// every package that COULD reference the symbol).
func BenchmarkDBPostingsFor_HotSymbol(b *testing.B) {
	for _, n := range []int{100, 1000, 8500} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			const locsPerSrc = 8 // ~72,000/8,500, the field report's own ratio
			db, targetPkg, targetID := populatePostingsDB(b, n, locsPerSrc)
			ctx := context.Background()
			wantResults := n * locsPerSrc

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				recs, err := db.PostingsFor(ctx, targetPkg, targetID)
				if err != nil {
					b.Fatalf("PostingsFor: %v", err)
				}
				total := 0
				for _, r := range recs {
					total += len(r.Locations)
				}
				if total != wantResults {
					b.Fatalf("PostingsFor returned %d locations, want %d", total, wantResults)
				}
			}
		})
	}
}
