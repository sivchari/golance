package golance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// gitWorktreeModule writes a synthetic single-module workspace, commits it
// to a fresh git repository, and adds a second worktree of it on its own
// branch. It returns the main worktree's root, the second worktree's root,
// and locs (captured against the main worktree's copy of the sources; the
// second worktree's files are byte-identical right after checkout).
func gitWorktreeModule(t *testing.T) (mainRoot, otherRoot string, locs e2eLocs) {
	t.Helper()
	mainRoot, locs = writeE2EModule(t)

	gitInit(t, mainRoot)
	gitAddAll(t, mainRoot)
	gitCommitInitial(t, mainRoot)

	otherRoot = gitWorktreeAdd(t, mainRoot)

	return mainRoot, otherRoot, locs
}

// runGitCmd runs cmd (already fully constructed by the caller — see
// gitInit/gitAddAll/gitCommitInitial/gitWorktreeAdd, each of which passes
// exec.Command an all-literal argument list, the one genuinely dynamic
// case — gitWorktreeAdd's target directory — aside) with dir as its
// working directory, failing t on error.
func runGitCmd(t *testing.T, dir string, cmd *exec.Cmd) {
	t.Helper()
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(cmd.Args[1:], " "), err, out)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGitCmd(t, dir, exec.Command("git", "init", "-q"))
}

func gitAddAll(t *testing.T, dir string) {
	t.Helper()
	runGitCmd(t, dir, exec.Command("git", "-c", "user.email=e2e@golance.test", "-c", "user.name=e2e", "add", "-A"))
}

func gitCommitInitial(t *testing.T, dir string) {
	t.Helper()
	runGitCmd(t, dir, exec.Command("git", "-c", "user.email=e2e@golance.test", "-c", "user.name=e2e", "commit", "-q", "-m", "initial"))
}

// gitWorktreeAdd adds a new worktree on branch "other" as a sibling
// directory of dir (git resolves the literal "../wt-other" against the
// command's working directory) and returns its absolute path. Using a
// constant relative target keeps every exec.Command argument a literal;
// the sibling lands next to dir under the test's temp area, never inside
// the scanned module tree.
func gitWorktreeAdd(t *testing.T, dir string) string {
	t.Helper()
	runGitCmd(t, dir, exec.Command("git", "worktree", "add", "-q", "-b", "other", "../wt-other", "HEAD"))
	return filepath.Join(filepath.Dir(dir), "wt-other")
}

// definitionAt requests textDocument/definition at pos in file and returns
// the (non-empty) result, failing t otherwise.
func definitionAt(t *testing.T, c *lspClient, file string, pos protocol.Position) protocol.LocationSlice {
	t.Helper()
	return c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}, e2eRequestBudget)
}

// startAndAwaitIndex starts a golance session for root (sharing fakeHome),
// opens appFile, and blocks until the session's index becomes ready, or
// e2eIndexBudget elapses. It returns the client, the build's parsed stats
// (see parseIndexStats — callers that want to assert this build avoided
// re-type-checking something should use stats.typeChecked, not elapsed),
// and the elapsed wall-clock time from initialize to index-ready, kept
// only for t.Logf: wall-clock time is not a reliable signal on a loaded CI
// runner, so e2eIndexBudget is a generous upper bound (bounding "did not
// hang forever"), never a tight one used to infer what the build actually
// did.
func startAndAwaitIndex(t *testing.T, root, fakeHome, appFile string) (c *lspClient, stats indexStats, elapsed time.Duration) {
	t.Helper()
	c = startClientIn(t, root, fakeHome)
	c.initialize(t, root)
	c.openFile(t, appFile)
	start := time.Now()
	msg := c.waitForIndexReady(t)
	elapsed = time.Since(start)
	return c, parseIndexStats(t, msg), elapsed
}

// TestE2E_WorktreeSharesIndex verifies that a second golance session opened
// against a git worktree of an already-indexed repository reuses the first
// session's CAS content (internal/server's repoKey-keyed casDir) instead of
// re-type-checking anything, and that an edit made only in that second
// worktree is incrementally reindexed without touching the first
// worktree's own per-root index.
//
// Unlike the pre-CAS design, the second worktree's own per-root index
// database (indexDBFile) is always private — never shared — so it still
// runs its own index build on first open; what the CAS buys it is that
// this build never re-type-checks anything the first worktree's build
// already processed, only resolves CAS hits and writes its own small
// per-root pointer/index entries (see startAndAwaitIndex's stats.typeChecked
// assertion below).
func TestE2E_WorktreeSharesIndex(t *testing.T) {
	skipUnlessE2E(t)

	mainRoot, otherRoot, locs := gitWorktreeModule(t)

	fakeHome := t.TempDir()

	// Worktree A: an ordinary cold-start session that builds its own
	// per-root index (and populates the shared CAS) from scratch.
	a, _, _ := startAndAwaitIndex(t, mainRoot, fakeHome, locs.appFile)

	got := definitionAt(t, a, locs.appFile, locs.sumCallInApp)
	if len(got) != 1 {
		t.Fatalf("worktree A: want exactly 1 definition location, got %d: %+v", len(got), got)
	}

	a.stop(t)

	// Worktree B: a second session, same repository (byte-identical
	// content, different worktree root), different absolute root. It still
	// runs its own index build (its per-root index database is private —
	// see startAndAwaitIndex's doc), but every package's content was
	// already processed for worktree A, so this build must complete via CAS
	// hits alone — asserted directly on the build's own reported stats
	// below, not inferred from how fast it happened to run; e2eIndexBudget
	// here is only an upper bound against the build hanging outright.
	otherAppFile := strings.Replace(locs.appFile, mainRoot, otherRoot, 1)
	b, bStats, elapsed := startAndAwaitIndex(t, otherRoot, fakeHome, otherAppFile)
	t.Logf("worktree B: build finished in %s (%+v)", elapsed, bStats)
	if bStats.typeChecked != 0 {
		t.Errorf("worktree B: type-checked %d package(s), want 0 (every package's content was already processed for worktree A; this build must resolve via CAS hits alone)", bStats.typeChecked)
	}

	got = definitionAt(t, b, otherAppFile, locs.sumCallInApp)
	if len(got) != 1 {
		t.Fatalf("worktree B: want exactly 1 definition location from the shared CAS, got %d: %+v", len(got), got)
	}
	otherUtilFile := strings.Replace(locs.utilFile, mainRoot, otherRoot, 1)
	if gotPath := got[0].URI.FsPath(); gotPath != otherUtilFile {
		t.Fatalf("worktree B: definition file = %s, want %s (worktree B's own absolute path, not worktree A's)", gotPath, otherUtilFile)
	}

	// Editing worktree B alone must incrementally reindex just that change
	// into its own per-root index (and the shared CAS): a brand-new
	// exported symbol becomes findable via workspace/symbol without ever
	// touching worktree A.
	otherUtilSrc := strings.Replace(locs.utilSrc, "func Sum(a, b int) int {\n\treturn a + b\n}\n",
		"func Sum(a, b int) int {\n\treturn a + b\n}\n\n// Product multiplies two ints.\nfunc Product(a, b int) int {\n\treturn a * b\n}\n", 1)
	b.openFile(t, otherUtilFile)
	b.changeFile(t, otherUtilFile, 2, otherUtilSrc)
	b.notify(t, protocol.MethodTextDocumentDidSave, &protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(otherUtilFile)},
	})

	deadline := time.Now().Add(e2eRequestBudget)
	for {
		resp := b.call(t, protocol.MethodWorkspaceSymbol, &protocol.WorkspaceSymbolParams{Query: "Product"}, e2eRequestBudget)
		if len(resp.Error) == 0 {
			var syms protocol.SymbolInformationSlice
			if err := protocol.Unmarshal(resp.Result, &syms); err == nil && len(syms) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("worktree B: incremental reindex did not surface the new Product symbol within %s", e2eRequestBudget)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The original worktree A file on disk is untouched: worktree A's own
	// database entry for lib/util must still be the pre-edit content.
	mainUtilSrc, err := os.ReadFile(filepath.Clean(locs.utilFile))
	if err != nil {
		t.Fatalf("read worktree A util.go: %v", err)
	}
	if strings.Contains(string(mainUtilSrc), "Product") {
		t.Fatal("worktree A's on-disk util.go was mutated by worktree B's edit")
	}
}

// TestE2E_WorktreeSimultaneousStartup verifies that two worktrees of the
// same repository can build their facts indexes at the same time without
// contention: unlike the pre-CAS design (one process-wide exclusive lock
// on a single shared database, forcing worktree A to exit before worktree
// B could even open it), the CAS is lock-free and each worktree's own
// per-root index database is private, so there is nothing for the two
// sessions to contend over.
func TestE2E_WorktreeSimultaneousStartup(t *testing.T) {
	skipUnlessE2E(t)

	mainRoot, otherRoot, locs := gitWorktreeModule(t)
	otherAppFile := strings.Replace(locs.appFile, mainRoot, otherRoot, 1)

	fakeHome := t.TempDir()

	// Start both sessions concurrently — neither waits for the other to
	// initialize, open a file, or finish indexing.
	a := startClientIn(t, mainRoot, fakeHome)
	b := startClientIn(t, otherRoot, fakeHome)
	a.initialize(t, mainRoot)
	b.initialize(t, otherRoot)
	a.openFile(t, locs.appFile)
	b.openFile(t, otherAppFile)

	resA := make(chan protocol.LocationSlice, 1)
	resB := make(chan protocol.LocationSlice, 1)
	go func() {
		a.waitForIndexReady(t)
		resA <- definitionAt(t, a, locs.appFile, locs.sumCallInApp)
	}()
	go func() {
		b.waitForIndexReady(t)
		resB <- definitionAt(t, b, otherAppFile, locs.sumCallInApp)
	}()

	deadline := time.After(e2eIndexBudget)
	var gotA, gotB protocol.LocationSlice
	haveA, haveB := false, false
	for !haveA || !haveB {
		select {
		case gotA = <-resA:
			haveA = true
		case gotB = <-resB:
			haveB = true
		case <-deadline:
			t.Fatal("simultaneous worktree startup did not complete within e2eIndexBudget")
		}
	}

	if len(gotA) != 1 {
		t.Errorf("worktree A: want exactly 1 definition location, got %d: %+v", len(gotA), gotA)
	}
	if len(gotB) != 1 {
		t.Errorf("worktree B: want exactly 1 definition location, got %d: %+v", len(gotB), gotB)
	}
}

// TestE2E_BranchSwitchNoRetypecheck verifies that switching a single
// worktree's content back to a version already seen (a stand-in for `git
// checkout` between two branches) does not re-type-check anything the
// second time around: the shared CAS already holds every package's blob
// for that exact content from the first time this worktree built it, so
// the third session's build (content reverted to the original) resolves
// entirely via CAS hits.
//
// This drives three sequential sessions against the same root, rewriting
// one file of a synthetic "heavy" package (writeHeavyPackage — many files,
// so a real recheck of the whole package is representative of the
// real-world case that motivated this design: a large generated-model
// package where any single file's edit forces the whole package to be
// reprocessed) directly between them, equivalent to the on-disk effect of
// a branch checkout without needing a real git history for this specific
// property: A (original content, a real build) -> B (a genuine edit,
// forcing a real re-type-check of the heavy package) -> A again (revert, a
// CAS-hit-only build).
//
// What matters is asserted directly on each build's own reported stats
// (see startAndAwaitIndex/indexStats), not inferred from wall-clock time:
// a shared CI runner's load makes elapsed-time comparisons an unreliable
// proxy for "did this build actually type-check the heavy package again."
// heavy is otherwise unimported by anything else in the workspace (see
// writeE2EModule's doc), so it is the only package whose own recheck could
// ever show up in these stats.
func TestE2E_BranchSwitchNoRetypecheck(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EModule(t)
	heavyFile, originalHeavySrc := writeHeavyPackage(t, root, "heavy", 500)
	fakeHome := t.TempDir()

	editedHeavySrc := strings.Replace(originalHeavySrc, "package heavy", "package heavy\n\n// edited marks a real content change.\nvar edited = true", 1)

	// Session A: original content, a genuine first-ever build. This also
	// primes the graph cache (go.mod/go.sum unchanged for the rest of this
	// test), so sessions B and C below pay the same fixed `go list`-free
	// startup cost.
	a, aStats, firstElapsed := startAndAwaitIndex(t, root, fakeHome, locs.appFile)
	t.Logf("session A: first build finished in %s (%+v)", firstElapsed, aStats)
	if got := definitionAt(t, a, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session A: want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	a.stop(t)

	// Session B: a genuine edit to the heavy package — new content this
	// CAS has never seen, forcing a real re-type-check of the whole
	// package.
	if err := os.WriteFile(heavyFile, []byte(editedHeavySrc), 0o600); err != nil {
		t.Fatalf("edit %s: %v", heavyFile, err)
	}
	b, bStats, editElapsed := startAndAwaitIndex(t, root, fakeHome, locs.appFile)
	t.Logf("session B: build finished in %s (%+v)", editElapsed, bStats)
	if bStats.typeChecked != 1 {
		t.Errorf("session B: type-checked %d package(s), want exactly 1 (heavy, the only package whose content changed)", bStats.typeChecked)
	}
	if got := definitionAt(t, b, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session B: want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	b.stop(t)

	// Session A again: revert the heavy package to the exact original
	// content. Its (content, dependency-API) combination now matches
	// exactly what session A already built, so this must resolve via a
	// CAS hit alone — no type-check.
	if err := os.WriteFile(heavyFile, []byte(originalHeavySrc), 0o600); err != nil {
		t.Fatalf("revert %s: %v", heavyFile, err)
	}
	c, cStats, revertElapsed := startAndAwaitIndex(t, root, fakeHome, locs.appFile)
	t.Logf("session A (reverted): build finished in %s (%+v)", revertElapsed, cStats)
	if cStats.typeChecked != 0 {
		t.Errorf("session A (reverted): type-checked %d package(s), want 0 (heavy's reverted content was already built by session A, so this must resolve via a CAS hit alone)", cStats.typeChecked)
	}
	if got := definitionAt(t, c, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session A (reverted): want exactly 1 definition location, got %d: %+v", len(got), got)
	}
}
