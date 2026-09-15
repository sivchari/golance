package server

import (
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// TestRefreshIndexResolver_InstallsFreshResolver verifies
// refreshIndexResolver's ordinary case: given the *indexState setWorkspace
// itself just Load()ed, it installs a new one wrapping the SAME db/cas
// handles with a Resolver rebuilt against the new snapshot.
func TestRefreshIndexResolver_InstallsFreshResolver(t *testing.T) {
	s, snap, root := newTestServer(t)
	cur := s.idx.Load()
	if cur == nil {
		t.Fatal("s.idx.Load() = nil, want the index newTestServer installed")
	}

	s.refreshIndexResolver(root, snap, cur)

	got := s.idx.Load()
	if got == cur {
		t.Fatal("refreshIndexResolver did not install a new *indexState")
	}
	if got.db != cur.db || got.cas != cur.cas {
		t.Error("refreshIndexResolver's replacement indexState must reuse the same db/cas handles, only its Resolver rebuilt")
	}
}

// TestRefreshIndexResolver_SkipsWhenIndexAlreadySuperseded is the regression
// test for the race setWorkspace's own doc describes: revalidateIndex's
// full-rebuild branch (indexer.go) can discard and Close an *indexState —
// guarded by s.idxMu, entirely independent of setWorkspaceMu — and install
// its own replacement between setWorkspace's s.idx.Load() and this call's
// own Store. Simulated here by installing a second, independent indexState
// (as if that rebuild had already finished) before calling
// refreshIndexResolver with the now-stale snapshot a concurrent setWorkspace
// would have captured: the compare-and-swap must detect s.idx no longer
// holds cur and leave the rebuild's own indexState alone, rather than
// clobbering it with a Resolver wrapping a database the rebuild may have
// already closed.
func TestRefreshIndexResolver_SkipsWhenIndexAlreadySuperseded(t *testing.T) {
	s, snap, root := newTestServer(t)
	cur := s.idx.Load()
	if cur == nil {
		t.Fatal("s.idx.Load() = nil, want the index newTestServer installed")
	}

	rebuiltDBPath := filepath.Join(t.TempDir(), "rebuilt-index.db")
	rebuiltCAS := openTestCAS(t)
	buildTestIndexDB(t, snap, rebuiltDBPath, rebuiltCAS)
	rebuiltDB, err := store.Open(rebuiltDBPath)
	if err != nil {
		t.Fatalf("store.Open(rebuilt): %v", err)
	}
	t.Cleanup(func() { _ = rebuiltDB.Close() })
	rebuilt := &indexState{db: rebuiltDB, cas: rebuiltCAS, resolver: s.newResolver(rebuiltDB, rebuiltCAS, snap, RelativeIndexPaths(root))}
	s.idx.Store(rebuilt)

	// cur's own db is now the "closed by a concurrent rebuild" stand-in:
	// close it to make installing a Resolver over it observably wrong if
	// the CAS below fails to skip.
	if err := cur.db.Close(); err != nil {
		t.Fatalf("close cur.db (simulating the rebuild's own Close): %v", err)
	}

	s.refreshIndexResolver(root, snap, cur)

	if got := s.idx.Load(); got != rebuilt {
		t.Error("refreshIndexResolver clobbered the concurrently installed indexState with a stale re-wrap of an already-closed db")
	}
}
