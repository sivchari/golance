package server

import (
	"context"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// TestDepCacheHolder_Importer_EvictsWholeCacheOncePastByteCap is the
// memory-bound sanity check for engineImporter.decodeRoot's own decode
// target: depCache's (fset, cache) pair, capped by maxDepCacheBytes (see its
// own doc). Once the cache's own blob-byte sum exceeds the cap, the very
// next importer() call must swap in a fresh, empty pair rather than let it
// grow unbounded — the same reset exportDecodeCap performs for
// internal/depcheck's own decode cache (see
// TestExportResolver_Resolve_WorkingSetAboveShrunkCapStaysResident), now
// exercised on the tier a root package's persisted export data (decodeRoot)
// decodes into. maxDepCacheBytes is lowered for this test alone (see its own
// doc for why it is a var, not a const) rather than generating fixture
// export data anywhere near its real 512MiB default.
func TestDepCacheHolder_Importer_EvictsWholeCacheOncePastByteCap(t *testing.T) {
	holder, exportProvider := buildColdGateDepCache(t, func() bool { return true })

	ctx := context.Background()
	cp, err := exportProvider.Package(ctx, "example.com/depcheckmod/dep")
	if err != nil {
		t.Fatalf("Package(dep): %v", err)
	}
	blob, err := typecheck.WriteExport(cp.Types(), exportProvider.FileSet())
	if err != nil {
		t.Fatalf("WriteExport(dep): %v", err)
	}

	if _, err := holder.decodeExport("example.com/depcheckmod/dep", blob); err != nil {
		t.Fatalf("decodeExport: %v", err)
	}
	before := holder.cache.Bytes()
	if before <= 0 {
		t.Fatalf("cache.Bytes() = %d after decoding a real blob, want > 0", before)
	}

	old := maxDepCacheBytes
	maxDepCacheBytes = before - 1 // force the next importer() call to see it exceeded
	t.Cleanup(func() { maxDepCacheBytes = old })

	oldCache := holder.cache
	holder.importer()
	if holder.cache == oldCache {
		t.Error("importer() did not swap in a fresh cache once past maxDepCacheBytes")
	}
	if got := holder.cache.Bytes(); got != 0 {
		t.Errorf("cache.Bytes() after eviction = %d, want 0 (a fresh, empty cache)", got)
	}
}
