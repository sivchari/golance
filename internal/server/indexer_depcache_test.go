package server

import "testing"

// TestShouldDiscardDepCache covers shouldDiscardDepCache's own decision
// table: only a full rebuild (rebuild=true) that actually installed a fresh
// index (idx != nil) and was not about to be retried against a locked path
// (locked=true) should discard the workspace's depCache. See
// shouldDiscardDepCache's own doc for why each other combination is a
// no-op.
func TestShouldDiscardDepCache(t *testing.T) {
	someIdx := &indexState{}
	tests := []struct {
		name    string
		rebuild bool
		locked  bool
		idx     *indexState
		want    bool
	}{
		{"full rebuild installed a fresh index", true, false, someIdx, true},
		{"cold start (not a rebuild) installed a fresh index", false, false, someIdx, false},
		{"full rebuild found the path locked, retry pending", true, true, nil, false},
		{"full rebuild left the index unavailable", true, false, nil, false},
		{"cold start found the path locked, retry pending", false, true, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDiscardDepCache(tt.rebuild, tt.locked, tt.idx); got != tt.want {
				t.Errorf("shouldDiscardDepCache(%v, %v, idx=%v) = %v, want %v", tt.rebuild, tt.locked, tt.idx != nil, got, tt.want)
			}
		})
	}
}

// TestDepCacheHolder_Reset verifies depCacheHolder.reset's own contract: it
// discards every decoded *types.Package and cached decode failure by
// swapping in a fresh (fset, cache) pair, in place — the same object other
// holders of *depCacheHolder (engineImporter.depCache, baked into
// ws.engine's own Importer closure at setWorkspace construction time)
// continue to reference afterward.
func TestDepCacheHolder_Reset(t *testing.T) {
	d := newDepCacheHolder(stubExportSource{}, nil, nil, func() bool { return true })

	if _, err := d.decodeExport("example.com/bogus", []byte("not real export data")); err == nil {
		t.Fatal("decodeExport(garbage) succeeded unexpectedly; want a decode error recorded in the cache")
	}
	oldFset, oldCache := d.fset, d.cache
	if oldCache.FailedLen() != 1 {
		t.Fatalf("cache.FailedLen() = %d before reset, want 1", oldCache.FailedLen())
	}

	d.reset()

	if d.fset == oldFset {
		t.Error("reset() left the old *token.FileSet in place, want a fresh one")
	}
	if d.cache == oldCache {
		t.Error("reset() left the old *typecheck.Cache in place, want a fresh one")
	}
	if d.cache.FailedLen() != 0 {
		t.Errorf("cache.FailedLen() after reset() = %d, want 0", d.cache.FailedLen())
	}
}

// stubExportSource is a typecheck.ExportSource that never has any data,
// enough for TestDepCacheHolder_Reset's decodeExport call, which supplies
// its own (invalid) export bytes directly rather than routing through this
// source.
type stubExportSource struct{}

func (stubExportSource) ExportData(string) ([]byte, bool, error) { return nil, false, nil }

// TestServer_DiscardStaleDepCache verifies discardStaleDepCache's own two
// cases: it resets the current workspace's depCache when one is installed,
// and is a silent no-op when s.workspace() is nil (Stop raced it, or the
// session never got past its initial graph load).
func TestServer_DiscardStaleDepCache(t *testing.T) {
	t.Run("resets the installed workspace's depCache", func(t *testing.T) {
		s, _ := newTestServerNoIndex(t)
		ws := s.workspace()
		if _, err := ws.depCache.decodeExport("example.com/bogus", []byte("garbage")); err == nil {
			t.Fatal("decodeExport(garbage) succeeded unexpectedly")
		}
		if ws.depCache.cache.FailedLen() != 1 {
			t.Fatalf("cache.FailedLen() = %d before discardStaleDepCache, want 1", ws.depCache.cache.FailedLen())
		}

		s.discardStaleDepCache()

		if ws.depCache.cache.FailedLen() != 0 {
			t.Errorf("cache.FailedLen() after discardStaleDepCache() = %d, want 0", ws.depCache.cache.FailedLen())
		}
	})

	t.Run("no-op when no workspace is installed", func(t *testing.T) {
		s := &Server{}
		s.discardStaleDepCache() // must not panic
	})
}
