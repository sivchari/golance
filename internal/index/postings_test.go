package index

import (
	"context"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// refKey identifies one reference by its target and location, comparable
// across a unit's facts-blob-decoded [store.Ref] and a
// [store.DB.PostingsFor]-decoded [store.PostingLocation]/[store.PostingRecord]
// pair.
type refKey struct {
	toPkgHash, toIDHash uint64
	file                string
	line, col, endCol   uint32
}

// factsRefs decodes every ref record in pkgPath's current facts blob into a
// refKey set.
func factsRefs(t *testing.T, db *store.DB, cas *store.CAS, pkgPath string) map[refKey]int {
	t.Helper()
	out := make(map[refKey]int)
	viewFacts(t, db, cas, pkgPath, func(v *store.View) {
		for i := 0; i < v.RefsCount(); i++ {
			r, err := v.RefAt(i)
			if err != nil {
				t.Fatalf("RefAt(%d): %v", i, err)
			}
			path, err := v.FileAt(int(r.FileIdx()))
			if err != nil {
				t.Fatalf("FileAt(%d): %v", r.FileIdx(), err)
			}
			out[refKey{r.ToPkgHash(), r.ToSymbolIDHash(), path, r.Line(), r.Col(), r.EndCol()}]++
		}
	})
	return out
}

// postingsRefsFrom decodes db's postings index entries contributed by
// srcPkgHash into the same refKey shape as factsRefs, scanning every
// (toPkgHash, toIDHash) pair actually present in wantTargets (facts alone
// does not expose "every target ever posted", so the test drives this from
// factsRefs' own keys instead of a separate posting-bucket enumeration).
func postingsRefsFrom(t *testing.T, db *store.DB, srcPkgHash uint64, wantTargets map[refKey]int) map[refKey]int {
	t.Helper()
	seenTargets := make(map[[2]uint64]bool)
	out := make(map[refKey]int)
	for k := range wantTargets {
		target := [2]uint64{k.toPkgHash, k.toIDHash}
		if seenTargets[target] {
			continue
		}
		seenTargets[target] = true

		recs, err := db.PostingsFor(context.Background(), k.toPkgHash, k.toIDHash)
		if err != nil {
			t.Fatalf("PostingsFor(%d, %d): %v", k.toPkgHash, k.toIDHash, err)
		}
		for _, rec := range recs {
			if rec.SrcPkgHash != srcPkgHash {
				continue
			}
			for _, loc := range rec.Locations {
				out[refKey{k.toPkgHash, k.toIDHash, loc.File, loc.Line, loc.Col, loc.EndCol}]++
			}
		}
	}
	return out
}

// TestBuild_PostingsMatchFactsRefsExactly cross-checks, per built unit, that
// the reverse reference index (internal/store's bucketRefPostings, queried
// via [store.DB.PostingsFor]) records EXACTLY the same (target, location)
// pairs that unit's own facts blob's refs table does — same multiset, same
// counts — confirming applyPostings' encode/write path (see
// internal/store/postings.go) faithfully mirrors what extractFacts's addRef
// already computes for the facts blob itself, per the design's "this adds
// encoding + writes, not new analysis" requirement.
func TestBuild_PostingsMatchFactsRefsExactly(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("Errors = %d, want 0", stats.Errors)
	}

	for _, pkgPath := range []string{pkgLeaf, pkgMid, pkgTop} {
		facts := factsRefs(t, db, cas, pkgPath)
		if len(facts) == 0 {
			continue // leaf itself makes no outgoing refs; nothing to cross-check.
		}
		postings := postingsRefsFrom(t, db, store.Hash(pkgPath), facts)

		if len(facts) != len(postings) {
			t.Errorf("%s: %d distinct facts refs, %d distinct postings refs, want equal\nfacts=%+v\npostings=%+v", pkgPath, len(facts), len(postings), facts, postings)
			continue
		}
		for k, wantCount := range facts {
			if gotCount := postings[k]; gotCount != wantCount {
				t.Errorf("%s: ref %+v appears %d times in facts, %d times in postings", pkgPath, k, wantCount, gotCount)
			}
		}
	}
}

// TestReindex_PostingsDropStaleReferenceExactly is the index-level
// counterpart of internal/store's
// TestDBPostingsManifestDrivenReplace: editing mid.go to no longer call
// leaf.Hello at all, then reindexing mid alone, must remove mid's posting
// for leaf.Hello from the reverse reference index — not merely leave it
// alongside whatever mid's edited body now references — exactly the
// incremental-correctness property applyPostings' manifest exists for (see
// its doc), exercised through Reindex's real processUnit/PutUnit path
// rather than store.DB directly.
func TestReindex_PostingsDropStaleReferenceExactly(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("initial Build: %v", err)
	}

	helloID := findSymbolByName(t, db, cas, pkgLeaf, "Hello")
	before, err := db.PostingsFor(ctx, store.Hash(pkgLeaf), helloID)
	if err != nil {
		t.Fatalf("PostingsFor(leaf.Hello) before reindex: %v", err)
	}
	if !hasSrc(before, store.Hash(pkgMid)) {
		t.Fatalf("PostingsFor(leaf.Hello) before reindex = %+v, want an entry from mid", before)
	}

	edited := []byte(`// Package mid depends on leaf.
package mid

import "strings"

// Shout returns an uppercase greeting for name, no longer calling leaf at
// all.
func Shout(name string) string {
	return strings.ToUpper("hello " + name)
}
`)
	reader := overlayReader(t, midSrcPath, edited)
	if _, err := Reindex(ctx, snap, db, cas, pkgMid, reader, Options{}); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	after, err := db.PostingsFor(ctx, store.Hash(pkgLeaf), helloID)
	if err != nil {
		t.Fatalf("PostingsFor(leaf.Hello) after reindex: %v", err)
	}
	if hasSrc(after, store.Hash(pkgMid)) {
		t.Errorf("PostingsFor(leaf.Hello) after reindex = %+v, want mid's stale posting gone", after)
	}
}

func hasSrc(recs []store.PostingRecord, srcPkgHash uint64) bool {
	for _, r := range recs {
		if r.SrcPkgHash == srcPkgHash {
			return true
		}
	}
	return false
}

// TestBuild_CASHitRepopulatesPostingsWithoutTypeChecking is the postings
// counterpart of TestBuild_RevertedContentIsCASHitNotTypeChecked: a CAS hit
// (a fresh *store.DB, e.g. a second session opening its own per-root index
// against a CAS another session already populated — see internal/server's
// switchToPrivateIndex) must repopulate the reverse reference index purely
// from the CAS blob's own already-extracted [store.PackageIndexEntries].
// Postings, added during facts extraction alongside Names/Methods/SymStrs
// (see extractFacts's doc), rides the exact same CAS-hit path those already
// did — casHitOutcome folds u.Index (decoded straight off the CAS blob)
// into the UnitEntry it returns, with no re-extraction involved — so this
// pins that a brand new db, opened against an already-warm CAS, ends up
// with the same postings a full Build would have produced, without paying
// for a single type-check.
func TestBuild_CASHitRepopulatesPostingsWithoutTypeChecking(t *testing.T) {
	snap := loadTestSnapshot(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	firstDB := openTestDB(t)
	if _, err := Build(ctx, snap, firstDB, cas, &Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	helloID := findSymbolByName(t, firstDB, cas, pkgLeaf, "Hello")

	// A second, brand new db (e.g. a fresh private index — see
	// internal/server's switchToPrivateIndex) against the SAME, already
	// populated cas.
	secondDB := openTestDB(t)
	stats, err := Build(ctx, snap, secondDB, cas, &Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if stats.TypeChecked != 0 {
		t.Fatalf("second Build TypeChecked = %d, want 0 (every package's content is already in the CAS)", stats.TypeChecked)
	}

	recs, err := secondDB.PostingsFor(ctx, store.Hash(pkgLeaf), helloID)
	if err != nil {
		t.Fatalf("PostingsFor(leaf.Hello) on the CAS-hit-populated db: %v", err)
	}
	if !hasSrc(recs, store.Hash(pkgTop)) || !hasSrc(recs, store.Hash(pkgMid)) {
		t.Errorf("PostingsFor(leaf.Hello) on the CAS-hit-populated db = %+v, want entries from both top and mid", recs)
	}
}
