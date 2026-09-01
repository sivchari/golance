package golance_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
)

// TestE2ERefreshOnWorkspaceReady reproduces the startup staleness bug
// (see internal/server/workspace.go's refreshOnWorkspaceReady): a client
// that declares workspace.inlayHint.refreshSupport must get a
// workspace/inlayHint/refresh push once the workspace becomes ready, even
// when nothing it did (no textDocument/didChange, not even a didOpen)
// triggered it.
//
// The reproduction needs setWorkspace to run twice for one session — once
// synchronously inside handleInitialize (installing a snapshot loaded from
// a stale on-disk cache) and once in the background right after, via
// revalidateGraph (installing the fresh reload) — since that is the exact
// transition a real editor's too-early request otherwise stays empty
// against. A first session builds and saves the on-disk import graph cache
// (internal/graph.SaveCache); go.mod's mtime is then bumped past the cache
// file's own, making internal/graph.Stale(root) true for a second session
// sharing the same cache, so its handleInitialize takes the
// warm-cache-but-stale path.
func TestE2ERefreshOnWorkspaceReady(t *testing.T) {
	skipUnlessE2E(t)

	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	writeE2EFile(t, root, "go.mod", "module example.com/refresh\n\ngo 1.23\n")
	writeE2EFile(t, root, "a/a.go", "package a\n\nfunc Add(x, y int) int { return x + y }\n")

	fakeHome := t.TempDir()

	// First session: an ordinary cold start, just to populate the on-disk
	// import graph cache for root.
	warm := startClientIn(t, root, fakeHome)
	warm.initialize(t, root)
	warm.stop(t)

	// Bump go.mod's mtime strictly past the cache file's own mtime, so
	// internal/graph.Stale(root) is true for the next session: LoadCache
	// still succeeds (serving the now-stale snapshot synchronously inside
	// handleInitialize), and handleInitialize launches revalidateGraph in
	// the background to reload and replace it — the second setWorkspace
	// call this test is actually exercising.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "go.mod"), future, future); err != nil {
		t.Fatalf("bump go.mod mtime: %v", err)
	}

	c := startClientIn(t, root, fakeHome)
	initializeWithInlayRefresh(t, c, root)
	defer c.stop(t)

	// No didOpen, no didChange: if this arrives, it can only be
	// revalidateGraph's setWorkspace call firing refreshOnWorkspaceReady.
	timeUntilServerRequest(t, c, protocol.MethodWorkspaceInlayHintRefresh)
}
