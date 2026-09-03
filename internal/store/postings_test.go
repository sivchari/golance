package store

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"testing"
)

// sortRecordsBySrc returns recs sorted by SrcPkgHash, so assertions do not
// depend on PostingsFor's own (unspecified) scan order.
func sortRecordsBySrc(recs []PostingRecord) []PostingRecord {
	out := append([]PostingRecord(nil), recs...)
	sort.Slice(out, func(i, j int) bool { return out[i].SrcPkgHash < out[j].SrcPkgHash })
	return out
}

// testUint32 converts n to uint32 for building small fixed test fixtures
// where n is always a known-small literal, panicking (unlike t.Fatalf, this
// satisfies gosec's G115 flow analysis) rather than silently truncating if
// that invariant is ever violated — mirrors this package's own u32len.
func testUint32(n uint64) uint32 {
	if n > math.MaxUint32 {
		panic(fmt.Sprintf("store: test fixture value %d exceeds uint32 range", n))
	}
	return uint32(n)
}

func TestDBPostingsForRoundTrip(t *testing.T) {
	db := openTestDB(t)
	const (
		targetPkg = 100
		targetID  = 200
		srcPkg    = 1
	)
	entry := UnitEntry{
		PkgHash: srcPkg,
		Pointer: UnitPointer{BlobKey: 7, ContentHash: 9},
		Index: PackageIndexEntries{
			Postings: []PostingEntry{
				{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "a.go", Line: 1, Col: 2, EndCol: 5},
				{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "a.go", Line: 3, Col: 1, EndCol: 4},
			},
		},
	}
	if err := db.PutUnit(&entry); err != nil {
		t.Fatalf("PutUnit() error = %v", err)
	}

	recs, err := db.PostingsFor(context.Background(), targetPkg, targetID)
	if err != nil {
		t.Fatalf("PostingsFor() error = %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("PostingsFor() = %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.SrcPkgHash != srcPkg {
		t.Errorf("SrcPkgHash = %d, want %d", rec.SrcPkgHash, srcPkg)
	}
	if rec.Bytes <= 0 {
		t.Errorf("Bytes = %d, want > 0", rec.Bytes)
	}
	want := []PostingLocation{
		{File: "a.go", Line: 1, Col: 2, EndCol: 5},
		{File: "a.go", Line: 3, Col: 1, EndCol: 4},
	}
	if !reflect.DeepEqual(rec.Locations, want) {
		t.Errorf("Locations = %+v, want %+v", rec.Locations, want)
	}
}

func TestDBPostingsForNoEntries(t *testing.T) {
	db := openTestDB(t)
	recs, err := db.PostingsFor(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("PostingsFor() error = %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("PostingsFor() = %+v, want no records", recs)
	}
}

func TestDBPostingsForMultipleSources(t *testing.T) {
	db := openTestDB(t)
	const (
		targetPkg = 100
		targetID  = 200
	)
	for _, src := range []uint64{1, 2, 3} {
		e := UnitEntry{
			PkgHash: src,
			Pointer: UnitPointer{BlobKey: src, ContentHash: src},
			Index: PackageIndexEntries{
				Postings: []PostingEntry{
					{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "x.go", Line: testUint32(src), Col: 1, EndCol: 2},
				},
			},
		}
		if err := db.PutUnit(&e); err != nil {
			t.Fatalf("PutUnit(src=%d) error = %v", src, err)
		}
	}

	recs, err := db.PostingsFor(context.Background(), targetPkg, targetID)
	if err != nil {
		t.Fatalf("PostingsFor() error = %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("PostingsFor() = %d records, want 3", len(recs))
	}
	recs = sortRecordsBySrc(recs)
	for i, src := range []uint64{1, 2, 3} {
		if recs[i].SrcPkgHash != src {
			t.Errorf("recs[%d].SrcPkgHash = %d, want %d", i, recs[i].SrcPkgHash, src)
		}
		if len(recs[i].Locations) != 1 || recs[i].Locations[0].Line != testUint32(src) {
			t.Errorf("recs[%d].Locations = %+v, want one location with Line %d", i, recs[i].Locations, src)
		}
	}
}

// TestDBPostingsManifestDrivenReplace covers applyPostings' core correctness
// requirement: reindexing srcPkgHash with an entirely different set of
// target references must remove every stale posting from its PREVIOUS
// write, not merely add the new ones alongside them — otherwise References
// would keep reporting a call site a source file no longer actually
// contains.
func TestDBPostingsManifestDrivenReplace(t *testing.T) {
	db := openTestDB(t)
	const srcPkg = 1
	const (
		oldTargetPkg, oldTargetID = 100, 200
		newTargetPkg, newTargetID = 300, 400
	)

	first := UnitEntry{
		PkgHash: srcPkg,
		Pointer: UnitPointer{BlobKey: 1, ContentHash: 1},
		Index: PackageIndexEntries{
			Postings: []PostingEntry{
				{TargetPkgHash: oldTargetPkg, TargetIDHash: oldTargetID, File: "a.go", Line: 1, Col: 1, EndCol: 2},
			},
		},
	}
	if err := db.PutUnit(&first); err != nil {
		t.Fatalf("PutUnit(first) error = %v", err)
	}
	if recs, err := db.PostingsFor(context.Background(), oldTargetPkg, oldTargetID); err != nil || len(recs) != 1 {
		t.Fatalf("PostingsFor(old) after first write = %+v, %v, want 1 record", recs, err)
	}

	// srcPkg is reindexed: its source no longer references oldTarget at
	// all, and now references a different symbol instead.
	second := UnitEntry{
		PkgHash: srcPkg,
		Pointer: UnitPointer{BlobKey: 2, ContentHash: 2},
		Index: PackageIndexEntries{
			Postings: []PostingEntry{
				{TargetPkgHash: newTargetPkg, TargetIDHash: newTargetID, File: "a.go", Line: 5, Col: 1, EndCol: 2},
			},
		},
	}
	if err := db.PutUnit(&second); err != nil {
		t.Fatalf("PutUnit(second) error = %v", err)
	}

	if recs, err := db.PostingsFor(context.Background(), oldTargetPkg, oldTargetID); err != nil || len(recs) != 0 {
		t.Errorf("PostingsFor(old) after reindex = %+v, %v, want no records (stale posting must be deleted)", recs, err)
	}
	recs, err := db.PostingsFor(context.Background(), newTargetPkg, newTargetID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("PostingsFor(new) after reindex = %+v, %v, want 1 record", recs, err)
	}
	if recs[0].SrcPkgHash != srcPkg || len(recs[0].Locations) != 1 || recs[0].Locations[0].Line != 5 {
		t.Errorf("PostingsFor(new) = %+v, want one location at line 5 from srcPkg %d", recs[0], srcPkg)
	}
}

// TestDBPostingsManifestDrivenReplaceLeavesOtherSourcesAlone covers the
// other half of applyPostings' exactness: reindexing one source package
// must not touch any OTHER source's postings for the same target, even
// though they share the same postingPrefix.
func TestDBPostingsManifestDrivenReplaceLeavesOtherSourcesAlone(t *testing.T) {
	db := openTestDB(t)
	const targetPkg, targetID = 100, 200
	const srcA, srcB = 1, 2

	for _, e := range []UnitEntry{
		{PkgHash: srcA, Pointer: UnitPointer{BlobKey: 1, ContentHash: 1}, Index: PackageIndexEntries{
			Postings: []PostingEntry{{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "a.go", Line: 1, Col: 1, EndCol: 2}},
		}},
		{PkgHash: srcB, Pointer: UnitPointer{BlobKey: 2, ContentHash: 2}, Index: PackageIndexEntries{
			Postings: []PostingEntry{{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "b.go", Line: 2, Col: 1, EndCol: 2}},
		}},
	} {
		if err := db.PutUnit(&e); err != nil {
			t.Fatalf("PutUnit(pkgHash=%d) error = %v", e.PkgHash, err)
		}
	}

	// Reindex srcA with no references left at all.
	if err := db.PutUnit(&UnitEntry{PkgHash: srcA, Pointer: UnitPointer{BlobKey: 3, ContentHash: 3}}); err != nil {
		t.Fatalf("PutUnit(reindex srcA) error = %v", err)
	}

	recs, err := db.PostingsFor(context.Background(), targetPkg, targetID)
	if err != nil {
		t.Fatalf("PostingsFor() error = %v", err)
	}
	if len(recs) != 1 || recs[0].SrcPkgHash != srcB {
		t.Fatalf("PostingsFor() after reindexing srcA away = %+v, want exactly srcB's posting untouched", recs)
	}
}

func TestDBPostingsPutUnitsBatch(t *testing.T) {
	db := openTestDB(t)
	const targetPkg, targetID = 100, 200
	entries := []UnitEntry{
		{PkgHash: 1, Pointer: UnitPointer{BlobKey: 1, ContentHash: 1}, Index: PackageIndexEntries{
			Postings: []PostingEntry{{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "a.go", Line: 1, Col: 1, EndCol: 2}},
		}},
		{PkgHash: 2, Pointer: UnitPointer{BlobKey: 2, ContentHash: 2}, Index: PackageIndexEntries{
			Postings: []PostingEntry{{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "b.go", Line: 2, Col: 1, EndCol: 2}},
		}},
	}
	if err := db.PutUnitsBatch(entries); err != nil {
		t.Fatalf("PutUnitsBatch() error = %v", err)
	}
	recs, err := db.PostingsFor(context.Background(), targetPkg, targetID)
	if err != nil || len(recs) != 2 {
		t.Fatalf("PostingsFor() = %+v, %v, want 2 records", recs, err)
	}
}

// TestDBPutUnitPointersBatchLeavesPostingsUntouched is
// TestDBPutUnitPointersBatchLeavesIndexUntouched's postings counterpart: a
// stat-only pointer refresh must never disturb the postings index.
func TestDBPutUnitPointersBatchLeavesPostingsUntouched(t *testing.T) {
	db := openTestDB(t)
	const targetPkg, targetID = 100, 200
	if err := db.PutUnit(&UnitEntry{
		PkgHash: 1,
		Pointer: UnitPointer{BlobKey: 10, ContentHash: 1},
		Index: PackageIndexEntries{
			Postings: []PostingEntry{{TargetPkgHash: targetPkg, TargetIDHash: targetID, File: "a.go", Line: 1, Col: 1, EndCol: 2}},
		},
	}); err != nil {
		t.Fatalf("PutUnit() error = %v", err)
	}

	refreshed := UnitPointer{BlobKey: 10, ContentHash: 1, Files: []FileStat{{Path: "/a.go", Size: 1, ModTimeNanos: 2}}}
	if err := db.PutUnitPointersBatch(map[uint64]UnitPointer{1: refreshed}); err != nil {
		t.Fatalf("PutUnitPointersBatch() error = %v", err)
	}

	recs, err := db.PostingsFor(context.Background(), targetPkg, targetID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("PostingsFor() after PutUnitPointersBatch = %+v, %v, want the original posting untouched", recs, err)
	}
}
