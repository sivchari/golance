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

// TestE2E_BranchSwitchSelfHealsWithoutFullRebuild verifies that switching a
// single worktree's content back and forth (a stand-in for `git checkout`
// between two branches, without needing a real git history for this
// specific property) is picked up correctly by a warm-reopened session —
// self-healed in place rather than left stale until an editor save.
//
// This drives three sequential sessions against the same root, rewriting
// one file of a synthetic "heavy" package (writeHeavyPackage — many files,
// so a real recheck of the whole package is representative of the
// real-world case that motivated this design: a large generated-model
// package where any single file's edit forces the whole package to be
// reprocessed) directly between them: A (original content, a real cold
// build) -> B (a genuine edit, self-healed by revalidateIndex's targeted
// repair, see internal/server/indexer.go) -> A again (revert, self-healed
// the same way).
//
// Sessions B and C are deliberately not awaited via startAndAwaitIndex's
// $/progress-based waitForIndexReady: each is a warm reopen of the same
// per-root facts index session A already built (tryWarmOpen succeeds), and
// heavy alone changing is a single-package stale set — small enough that
// revalidateIndex's targeted repair (repairIndexPackagesLocked) now fixes it
// in-process rather than falling back to a full indexer-subprocess rebuild
// (see indexRepairThreshold's own doc), so no $/progress notification is
// ever sent for either session. Correctness is instead observed directly
// through workspace/symbol on an exported marker function the edit adds
// (HeavyEdited): its presence/absence reflects whether the facts index's
// own view of heavy actually caught up with what changed on disk. This
// intentionally no longer distinguishes a CAS hit from a genuine
// retypecheck the way the original version of this test (subprocess STATS)
// did — internal/index's own Build/Reindex tests already cover that
// invariant directly; what matters here is that a warm session's facts
// self-heal without a save, exactly like TestE2E_WorktreeSelfHealsMissingPackageFacts
// verifies for the worktree-specific shared-cache variant of the same gap.
func TestE2E_BranchSwitchSelfHealsWithoutFullRebuild(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EModule(t)
	heavyFile, originalHeavySrc := writeHeavyPackage(t, root, "heavy", 500)
	fakeHome := t.TempDir()

	const heavyEditedMarker = "HeavyEdited"
	editedHeavySrc := strings.Replace(originalHeavySrc, "package heavy",
		"package heavy\n\n// HeavyEdited marks a real content change, added only by this test.\nfunc HeavyEdited() bool { return true }", 1)

	// Session A: original content, a genuine first-ever cold build (no
	// per-root facts index exists yet, so tryWarmOpen cannot succeed and
	// buildIndex always runs a real indexer subprocess regardless of
	// indexRepairThreshold). This also primes the graph cache (go.mod/go.sum
	// unchanged for the rest of this test), so sessions B and C below pay
	// the same fixed `go list`-free startup cost.
	a, aStats, firstElapsed := startAndAwaitIndex(t, root, fakeHome, locs.appFile)
	t.Logf("session A: first build finished in %s (%+v)", firstElapsed, aStats)
	if got := definitionAt(t, a, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session A: want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	waitForWorkspaceSymbolCount(t, a, heavyEditedMarker, 0, e2eRequestBudget)
	a.stop(t)

	// Session B: a genuine edit to the heavy package, then a warm reopen of
	// the same per-root facts index session A already built — self-healed by
	// revalidateIndex's targeted repair without ever saving through the
	// editor.
	if err := os.WriteFile(heavyFile, []byte(editedHeavySrc), 0o600); err != nil {
		t.Fatalf("edit %s: %v", heavyFile, err)
	}
	b := startClientIn(t, root, fakeHome)
	b.initialize(t, root)
	b.openFile(t, locs.appFile)
	if got := definitionAt(t, b, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session B: want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	waitForWorkspaceSymbolCount(t, b, heavyEditedMarker, 1, e2eRequestBudget)
	b.stop(t)

	// Session A again: revert the heavy package to the exact original
	// content, then another warm reopen — the marker function must
	// disappear from the facts index again, proving the repair genuinely
	// reflects heavy's current on-disk content each time, not merely a
	// one-way "something changed" flag.
	if err := os.WriteFile(heavyFile, []byte(originalHeavySrc), 0o600); err != nil {
		t.Fatalf("revert %s: %v", heavyFile, err)
	}
	c := startClientIn(t, root, fakeHome)
	c.initialize(t, root)
	c.openFile(t, locs.appFile)
	if got := definitionAt(t, c, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("session A (reverted): want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	waitForWorkspaceSymbolCount(t, c, heavyEditedMarker, 0, e2eRequestBudget)
	c.stop(t)
}

// waitForWorkspaceSymbolCount polls workspace/symbol for query until it
// returns exactly wantCount results, or timeout elapses — for asserting a
// symbol has become visible (wantCount > 0) or, symmetrically, has
// disappeared again (wantCount == 0) once a background self-heal is
// expected to have settled.
func waitForWorkspaceSymbolCount(t *testing.T, c *lspClient, query string, wantCount int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp := c.callRetryIndexUnavailable(t, protocol.MethodWorkspaceSymbol, &protocol.WorkspaceSymbolParams{Query: query}, timeout)
		if len(resp.Error) > 0 {
			t.Fatalf("workspace/symbol(%q) failed: %s", query, resp.Error)
		}
		var syms protocol.SymbolInformationSlice
		if err := protocol.Unmarshal(resp.Result, &syms); err != nil {
			t.Fatalf("unmarshal workspace/symbol(%q) result: %v", query, err)
		}
		if len(syms) == wantCount {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace/symbol(%q) returned %d result(s) after %s, want %d", query, len(syms), timeout, wantCount)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestE2E_WorktreeSelfHealsMissingPackageFacts is a regression test for the
// worktree bug this package's own commit fixed: a session that warm-opens
// its own already-built per-root facts index trusts the repository-shared
// graph cache (graph.Shared) immediately at startup, which can list a
// different package set than what is actually on disk for this worktree
// right now — reproduced here simply by adding a package to worktree B
// alone, after both the shared graph cache and worktree B's own facts index
// were last saved. Before the fix, the background revalidateGraph pass that
// self-heals the snapshot (kicked unconditionally for a shared cache, see
// loadWorkspaceAsync) never re-checked the facts index against the fresher
// snapshot it installs, so the new package's facts were never computed
// until something else (a save) happened to trigger a reindex — jumping
// into it, or finding references to it, stayed broken (an empty result,
// forever) rather than self-healing within any bounded time: handleReferences
// never surfaces a facts-read failure as a client-facing RPC error (it logs
// "xref: read facts for ...: store: not found" server-side and answers with
// an empty result instead), so definitionAt/waitForNonEmptyLocations'
// bounded retry-until-non-empty below is what actually distinguishes "self-
// healed" from "still broken," not any client-visible error.
//
// Deliberately never opens the new files (textDocument/didOpen) before
// querying: that would additionally exercise handleDidOpen's own
// self-heal (already covered by internal/server's own unit tests), muddying
// this test's specific coverage of the background revalidateGraph path —
// the actual trigger behind the original bug report.
func TestE2E_WorktreeSelfHealsMissingPackageFacts(t *testing.T) {
	skipUnlessE2E(t)

	mainRoot, otherRoot, locs := gitWorktreeModule(t)
	fakeHome := t.TempDir()

	// Worktree A: an ordinary cold-start session. Its build populates the
	// shared CAS and the repository-shared graph cache (graph.Shared),
	// neither of which yet knows about the package added to worktree B
	// below.
	a, _, _ := startAndAwaitIndex(t, mainRoot, fakeHome, locs.appFile)
	if got := definitionAt(t, a, locs.appFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("worktree A: want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	a.stop(t)

	// Worktree B's own first session: also a genuine cold start (its
	// per-root facts index does not exist yet), which builds and installs
	// it. This is the shared graph cache's first save from worktree B's own
	// perspective, so it now reflects worktree B's current (still
	// incomplete) package set.
	otherAppFile := strings.Replace(locs.appFile, mainRoot, otherRoot, 1)
	b, _, _ := startAndAwaitIndex(t, otherRoot, fakeHome, otherAppFile)
	if got := definitionAt(t, b, otherAppFile, locs.sumCallInApp); len(got) != 1 {
		t.Fatalf("worktree B: want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	b.stop(t)

	// Add two brand-new packages to worktree B alone, after both the
	// shared graph cache and worktree B's own per-root facts index were
	// last saved: neither knows about them yet.
	const newPkgSrc = `package newpkg

// Greet returns a greeting.
func Greet() string {
	return "hi"
}
`
	newPkgFile := writeE2EFile(t, otherRoot, "newpkg/newpkg.go", newPkgSrc)
	greetDecl := mustPos(t, newPkgSrc, "func Greet", "Greet")

	const usenewSrc = `package usenew

import "example.com/e2e/newpkg"

// Call invokes newpkg.Greet.
func Call() string {
	return newpkg.Greet()
}
`
	usenewFile := writeE2EFile(t, otherRoot, "usenew/usenew.go", usenewSrc)
	greetCall := mustPos(t, usenewSrc, "newpkg.Greet()", "Greet")

	// Worktree B, second session: warm-opens the same per-root facts index
	// built above (still missing newpkg/usenew entirely) over a graph
	// snapshot the shared cache also does not list them in yet — the exact
	// state a wrong/stale snapshot leaves behind. graph.Shared(otherRoot)
	// unconditionally kicks a background revalidateGraph pass on every such
	// start; the fix under test is that pass now also revalidating the
	// facts index against the fresh snapshot it installs, repairing the two
	// new packages in place without any save.
	//
	// Unlike startAndAwaitIndex's cold-start sessions above, this does not
	// wait for a $/progress "golance/index" end notification: tryWarmOpen
	// succeeds immediately here (worktree B's own per-root database already
	// exists), so no indexer subprocess ever runs and no such notification
	// is ever sent — neither for the (old, buggy) no-op revalidateIndex
	// outcome nor for the new in-process targeted repair, both of which
	// stay entirely within this process. definitionAt/waitForNonEmptyLocations's
	// own retry loop (up to e2eRequestBudget) is what actually waits out the
	// self-heal below.
	c := startClientIn(t, otherRoot, fakeHome)
	c.initialize(t, otherRoot)
	c.openFile(t, otherAppFile)
	defer c.stop(t)

	got := definitionAt(t, c, usenewFile, greetCall)
	if len(got) != 1 || got[0].URI.FsPath() != newPkgFile {
		t.Fatalf("worktree B: definition on newpkg.Greet() = %+v, want exactly 1 location in %s", got, newPkgFile)
	}

	refs := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(newPkgFile)},
			Position:     greetDecl,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}, e2eRequestBudget)
	var foundCallSite bool
	for _, l := range refs {
		if l.URI.FsPath() == usenewFile {
			foundCallSite = true
		}
	}
	if !foundCallSite {
		t.Fatalf("worktree B: references on Greet's declaration missing the call site in %s; got %+v", usenewFile, refs)
	}
}
