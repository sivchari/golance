package golance_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// privateIndexFiles returns every session-private index database file (see
// internal/server's privateIndexDBFile — named "index-<hash>.private-<id>.db",
// as opposed to the shared "index-<hash>.db") found under fakeHome's cache
// directory. cacheBaseDir's own OS-specific rule (os.UserCacheDir) is
// duplicated here rather than imported, since it is unexported; e2eEnv sets
// both HOME and XDG_CACHE_HOME to fakeHome so this only needs to know which
// one os.UserCacheDir actually honors per OS.
func privateIndexFiles(t *testing.T, fakeHome string) []string {
	t.Helper()
	cacheDir := filepath.Join(fakeHome, ".cache", "golance")
	if runtime.GOOS == "darwin" {
		cacheDir = filepath.Join(fakeHome, "Library", "Caches", "golance")
	}
	matches, err := filepath.Glob(filepath.Join(cacheDir, "*.private-*.db"))
	if err != nil {
		t.Fatalf("glob private index files: %v", err)
	}
	return matches
}

// TestE2E_SameWorkspaceConcurrentSessionsFallBackToPrivateIndex verifies
// the multi-editor scenario this design fixes: a second golance session
// opened against the SAME workspace root as an already-running first
// session (not a different git worktree — see TestE2E_WorktreeSharesIndex
// for that case) does not lose cross-reference functionality just because
// the shared per-root index database is locked by the first session. It
// falls back to its own session-private index and gets full functionality
// from it — definition and references both answer correctly — and that
// private index file is removed again once the session cleanly stops.
func TestE2E_SameWorkspaceConcurrentSessionsFallBackToPrivateIndex(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EModule(t)
	fakeHome := t.TempDir()

	// Session A: an ordinary cold start that builds and then holds the
	// shared per-root index's lock for the rest of this test.
	a := startClientIn(t, root, fakeHome)
	a.initialize(t, root)
	a.openFile(t, locs.appFile)
	a.waitForIndexReady(t)

	if got := definitionAt(t, a, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session A: want exactly 1 definition location, got %d: %+v", len(got), got)
	}

	if got := privateIndexFiles(t, fakeHome); len(got) != 0 {
		t.Fatalf("session A (the first, uncontended session) has private index file(s), want none: %v", got)
	}

	// Session B: a second session for the exact same root, started while A
	// is still running and holding the shared index's lock. It must still
	// reach full cross-reference functionality, via its own private index.
	b := startClientIn(t, root, fakeHome)
	b.initialize(t, root)
	b.openFile(t, locs.appFile)
	b.waitForIndexReady(t)

	if got := definitionAt(t, b, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session B: want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	refs := b.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilFile)},
			Position:     locs.sumDecl,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}, e2eRequestBudget)
	if len(refs) == 0 {
		t.Fatal("session B: textDocument/references returned nothing from its session-private index")
	}

	privateBefore := privateIndexFiles(t, fakeHome)
	if len(privateBefore) == 0 {
		t.Fatal("no session-private index file found under the fake cache dir; want session B to have fallen back to one")
	}

	b.stop(t)

	if got := privateIndexFiles(t, fakeHome); len(got) != 0 {
		t.Fatalf("session-private index file(s) still present after session B stopped cleanly: %v", got)
	}

	a.stop(t)
}
