package depexport

import (
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
)

// TestCache_MemoizeForRun_AvoidsRedundantRecheckAfterEviction pins
// Options.MemoizeForRun's own contract (see Cache's doc): once THIS Cache
// instance has resolved pkgPath, every later ExportData(pkgPath) call
// returns the SAME bytes without re-consulting provider at all, even after
// provider's own LRU has evicted pkgPath to make room for something else —
// closing the cross-call identity-split window a fresh, independent
// re-check (a REAL possibility once evicted — see genericsplit_test.go's
// own TestProvider_ClosureScope_PreventsGenericTypeIdentitySplit for the
// confirmed mechanism a repeat check can diverge by) leaves open.
// Cap: 1 guarantees eviction the moment a second, unrelated path is
// resolved, deterministically forcing the scenario without depending on
// incidental LRU ordering.
func TestCache_MemoizeForRun_AvoidsRedundantRecheckAfterEviction(t *testing.T) {
	meta := loadTestGraph(t)
	const (
		depPath = "example.com/depcheckmod/dep"
		// testingPath is independent of depPath (neither imports the
		// other — only dep's own _test.go variant pulls in "testing", not
		// dep itself): resolving it must not keep depPath pinned the way
		// resolving "user" (which directly imports dep) would, so Cap: 1
		// genuinely evicts depPath here rather than incidentally reusing
		// it as user's own already-cached dependency.
		testingPath = "testing"
	)

	for _, tc := range []struct {
		name    string
		memoize bool
	}{
		{name: "without MemoizeForRun", memoize: false},
		{name: "with MemoizeForRun", memoize: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := depcheck.NewProvider(meta, depcheck.Options{Cap: 1})
			cache := NewCache(nil, meta, provider, Options{MemoizeForRun: tc.memoize})

			blob1, ok, err := cache.ExportData(depPath)
			if err != nil || !ok {
				t.Fatalf("ExportData(%s): ok=%v err=%v", depPath, ok, err)
			}

			// Cap: 1 evicts depPath from provider's own LRU (transitively,
			// its whole closure) to make room for testingPath's own.
			if _, ok, err := cache.ExportData(testingPath); err != nil || !ok {
				t.Fatalf("ExportData(%s): ok=%v err=%v", testingPath, ok, err)
			}
			checkedAfterUser := provider.Checked()

			blob2, ok, err := cache.ExportData(depPath)
			if err != nil || !ok {
				t.Fatalf("second ExportData(%s): ok=%v err=%v", depPath, ok, err)
			}
			checkedAfterSecondDep := provider.Checked()

			rechecked := checkedAfterSecondDep > checkedAfterUser
			if tc.memoize && rechecked {
				t.Errorf("Checked() rose from %d to %d re-requesting dep with MemoizeForRun set: provider was re-consulted instead of serving the memoized bytes", checkedAfterUser, checkedAfterSecondDep)
			}
			if !tc.memoize && !rechecked {
				t.Errorf("Checked() stayed at %d re-requesting dep without MemoizeForRun: want a real re-check after Cap:1 evicted it (test no longer exercises eviction)", checkedAfterUser)
			}
			if tc.memoize && string(blob1) != string(blob2) {
				t.Error("MemoizeForRun set but the second ExportData(dep) call returned different bytes than the first")
			}
		})
	}
}
