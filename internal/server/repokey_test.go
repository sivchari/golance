package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitRepoWithWorktree creates a fresh git repository with one commit and a
// second linked worktree of it, returning both roots (symlink-resolved, so
// string comparisons against repoKey's own symlink-resolved output are
// meaningful on macOS's /var -> /private/var).
func gitRepoWithWorktree(t *testing.T) (mainRoot, otherRoot string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	mainRoot = filepath.Join(base, "main")
	if err := os.MkdirAll(mainRoot, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", mainRoot, err)
	}
	runGit(t, mainRoot, "init", "-q")
	runGit(t, mainRoot, "-c", "user.email=test@golance.test", "-c", "user.name=test", "commit", "-q", "--allow-empty", "-m", "init")
	otherRoot = filepath.Join(base, "other")
	runGit(t, mainRoot, "worktree", "add", "-q", "-b", "other", otherRoot, "HEAD")
	return mainRoot, otherRoot
}

// TestRepoKey_NonGitDirIsUnshared verifies that a plain (non-git) directory
// gets its own private key, unchanged from golance's pre-worktree-sharing
// behavior: repoKey falls back to root itself, and RelativeIndexPaths is
// false (so its CAS blobs and index keep storing absolute paths, matching
// every existing on-disk index for such a root).
func TestRepoKey_NonGitDirIsUnshared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	key, shared := repoKey(root)
	if shared {
		t.Errorf("repoKey(%s) shared = true, want false for a non-git directory", root)
	}
	if key != root {
		t.Errorf("repoKey(%s) key = %s, want root itself", root, key)
	}
	if RelativeIndexPaths(root) {
		t.Error("RelativeIndexPaths() = true for a non-git directory, want false")
	}
}

// TestRepoKey_WorktreesShareOneCASButNotIndexDB verifies that two
// worktrees of the same git repository resolve to the same repoKey (and so
// the same casDir — the shared, lock-free content-addressed layer), while
// still getting distinct indexDBFile paths (the small per-root index is
// always private, never shared — see indexDBFile's doc), and that both
// report RelativeIndexPaths = true.
func TestRepoKey_WorktreesShareOneCASButNotIndexDB(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mainRoot, otherRoot := gitRepoWithWorktree(t)

	k1, shared1 := repoKey(mainRoot)
	k2, shared2 := repoKey(otherRoot)
	if !shared1 || !shared2 {
		t.Fatalf("repoKey shared = (%v, %v), want (true, true) for two worktrees of one repository", shared1, shared2)
	}
	if k1 != k2 {
		t.Errorf("repoKey mismatch between worktrees: %s vs %s", k1, k2)
	}
	if casDir(mainRoot) != casDir(otherRoot) {
		t.Errorf("casDir mismatch between worktrees: %s vs %s", casDir(mainRoot), casDir(otherRoot))
	}
	if indexDBFile(mainRoot) == indexDBFile(otherRoot) {
		t.Error("indexDBFile is the same for two worktrees; want distinct per-root paths (never shared)")
	}
	if !RelativeIndexPaths(mainRoot) || !RelativeIndexPaths(otherRoot) {
		t.Error("RelativeIndexPaths() = false for a worktree, want true")
	}
}

// TestRepoKey_DistinctReposDontShare verifies that two unrelated git
// repositories (not worktrees of each other) get distinct CAS directories.
func TestRepoKey_DistinctReposDontShare(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	for _, r := range []string{rootA, rootB} {
		if err := os.MkdirAll(r, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", r, err)
		}
		runGit(t, r, "init", "-q")
		runGit(t, r, "-c", "user.email=test@golance.test", "-c", "user.name=test", "commit", "-q", "--allow-empty", "-m", "init")
	}
	if casDir(rootA) == casDir(rootB) {
		t.Error("casDir is the same for two unrelated repositories")
	}
	if indexDBFile(rootA) == indexDBFile(rootB) {
		t.Error("indexDBFile is the same for two unrelated repositories")
	}
}

// TestIndexDBFile_AlwaysPrivate verifies that indexDBFile depends only on
// root itself, never on repoKey: two different absolute roots always get
// different index database paths, whether or not they share a repository —
// there is no shared-file contention to avoid (see indexDBFile's doc), so
// there is nothing to probe or fall back from.
func TestIndexDBFile_AlwaysPrivate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rootA := t.TempDir()
	rootB := t.TempDir()
	if indexDBFile(rootA) == indexDBFile(rootB) {
		t.Error("indexDBFile is the same for two different roots, want distinct paths")
	}
}
