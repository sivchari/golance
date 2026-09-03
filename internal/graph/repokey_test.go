package graph

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realGOMODCACHE returns the module cache directory the running `go` binary
// actually resolves in this test's own real environment, for a test that
// sandboxes $HOME (moving Go's own default GOMODCACHE location) but still
// needs to resolve a real module-cache dependency without redownloading it.
func realGOMODCACHE(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOMODCACHE: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runGitCmd runs cmd with dir as its working directory, failing t on error.
// Mirrors internal/server/repokey_test.go's identical helper.
func runGitCmd(t *testing.T, dir string, cmd *exec.Cmd) {
	t.Helper()
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(cmd.Args[1:], " "), err, out)
	}
}

// gitModuleRepoWithWorktree creates a fresh git repository containing a
// tiny buildable Go module (one package, "pkg", importing nothing outside
// the standard library) with one commit, then adds a second linked
// worktree of it on branch "other" — both roots symlink-resolved so string
// comparisons against RepoKey's own symlink-resolved output are meaningful
// on macOS's /var -> /private/var. Returns both worktree roots.
func gitModuleRepoWithWorktree(t *testing.T) (mainRoot, otherRoot string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	mainRoot = filepath.Join(base, "main")
	if err := os.MkdirAll(filepath.Join(mainRoot, "pkg"), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", mainRoot, err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "go.mod"), []byte("module example.com/worktreefixture\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "pkg", "pkg.go"), []byte("package pkg\n\nfunc Hello() string { return \"hi\" }\n"), 0o600); err != nil {
		t.Fatalf("write pkg.go: %v", err)
	}

	runGitCmd(t, mainRoot, exec.Command("git", "init", "-q"))
	runGitCmd(t, mainRoot, exec.Command("git", "add", "."))
	runGitCmd(t, mainRoot, exec.Command("git", "-c", "user.email=test@golance.test", "-c", "user.name=test", "commit", "-q", "-m", "init"))
	runGitCmd(t, mainRoot, exec.Command("git", "worktree", "add", "-q", "-b", "other", "../wt-other", "HEAD"))
	otherRoot = filepath.Join(base, "wt-other")
	return mainRoot, otherRoot
}

// TestCache_SharedAcrossWorktrees verifies the core deliverable: a graph
// cache SaveCache writes from one worktree of a git repository is directly
// LoadCache-able from a DIFFERENT worktree of the same repository — same
// package set, and every path rejoined onto the SECOND worktree's own root
// rather than the first's (see toDiskPackages/fromDiskPackages) — so a
// brand-new sibling worktree gets an instantly-usable graph instead of its
// own cold `go list`.
func TestCache_SharedAcrossWorktrees(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	mainRoot, otherRoot := gitModuleRepoWithWorktree(t)

	if !Shared(mainRoot) || !Shared(otherRoot) {
		t.Fatalf("Shared = (%v, %v), want (true, true) for two worktrees of one repository", Shared(mainRoot), Shared(otherRoot))
	}
	if CacheFile(mainRoot) != CacheFile(otherRoot) {
		t.Fatalf("CacheFile mismatch between worktrees: %s vs %s", CacheFile(mainRoot), CacheFile(otherRoot))
	}

	patterns := []string{"./..."}
	snap, err := Load(Options{Dir: mainRoot}, patterns...)
	if err != nil {
		t.Fatalf("Load(mainRoot): %v", err)
	}
	if err := SaveCache(mainRoot, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache(mainRoot): %v", err)
	}

	loaded, ok := LoadCache(otherRoot, patterns, nil)
	if !ok {
		t.Fatal("LoadCache(otherRoot) = not ok, want the cache mainRoot just saved to be shared")
	}
	if len(loaded.Packages) != len(snap.Packages) {
		t.Fatalf("LoadCache(otherRoot) package count = %d, want %d", len(loaded.Packages), len(snap.Packages))
	}

	const pkgPath = "example.com/worktreefixture/pkg"
	pkg, ok := loaded.Package(pkgPath)
	if !ok {
		t.Fatalf("LoadCache(otherRoot) missing %s", pkgPath)
	}
	wantDir := filepath.Join(otherRoot, "pkg")
	if pkg.Dir != wantDir {
		t.Errorf("Package(%s).Dir = %s, want %s (rejoined onto otherRoot, not mainRoot)", pkgPath, pkg.Dir, wantDir)
	}
	if len(pkg.GoFiles) != 1 || pkg.GoFiles[0] != filepath.Join(otherRoot, "pkg", "pkg.go") {
		t.Errorf("Package(%s).GoFiles = %v, want [%s]", pkgPath, pkg.GoFiles, filepath.Join(otherRoot, "pkg", "pkg.go"))
	}
	if loaded.Dir() != otherRoot {
		t.Errorf("loaded Snapshot.Dir() = %s, want otherRoot %s", loaded.Dir(), otherRoot)
	}

	rootPkg, ok := loaded.Package("example.com/worktreefixture/pkg")
	if ok && strings.HasPrefix(rootPkg.Dir, mainRoot) {
		t.Errorf("Package(%s).Dir = %s still points at mainRoot, want it rejoined onto otherRoot", pkgPath, rootPkg.Dir)
	}
}

// TestCache_DependencyPathsStayAbsoluteAcrossWorktrees verifies that a
// dependency package's Dir/GoFiles — module-cache or GOROOT paths, already
// stable across every worktree of a repository (see toDiskPackages' doc) —
// come back byte-identical after a cross-worktree LoadCache, unlike a
// workspace-local package's paths (see TestCache_SharedAcrossWorktrees),
// which are rejoined onto the loading root.
func TestCache_DependencyPathsStayAbsoluteAcrossWorktrees(t *testing.T) {
	// Captured BEFORE sandboxing $HOME below, so this resolves the real,
	// already-populated module cache rather than one relative to the
	// about-to-be-fake $HOME.
	modcache := realGOMODCACHE(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// Point GOMODCACHE at the real, already-populated module cache instead
	// of letting it default under the now-sandboxed $HOME: this fixture's
	// whole point is exercising a real module-cache dependency (see
	// loadDepFixture), and redirecting GOMODCACHE would force a redundant
	// re-download into a throwaway directory t.TempDir() cannot always
	// clean up afterward (the module cache's own files are read-only).
	t.Setenv("GOMODCACHE", modcache)

	root, err := filepath.Abs(filepath.Join("testdata", "depfixture"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	// depfixture is not itself a git worktree; SaveCache/LoadCache's path
	// treatment does not depend on Shared() being true for this particular
	// assertion — it only depends on the dependency's Dir/GoFiles falling
	// outside root, which relPath already leaves untouched (see
	// toDiskPackages' doc) regardless of whether the cache file itself ends
	// up shared or private.
	snap := loadDepFixture(t)
	patterns := []string{"./..."}
	if err := SaveCache(root, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	loaded, ok := LoadCache(root, patterns, nil)
	if !ok {
		t.Fatal("LoadCache: ok=false after SaveCache")
	}
	depWant, ok := snap.Package(depfixtureDep)
	if !ok {
		t.Fatalf("Package(%s) missing from original snapshot", depfixtureDep)
	}
	depGot, ok := loaded.Package(depfixtureDep)
	if !ok {
		t.Fatalf("Package(%s) missing after LoadCache", depfixtureDep)
	}
	if depGot.Dir != depWant.Dir {
		t.Errorf("dependency Dir after round trip = %s, want unchanged %s", depGot.Dir, depWant.Dir)
	}
}

// TestCache_VersionBumpDiscardsSharedSnapshot verifies that a cache written
// under a previous cacheVersion — in particular a pre-v4 cache whose
// Dir/GoFiles are plain absolute strings for whichever single worktree
// wrote them (see cacheVersion's v4 doc) — is rejected outright rather than
// being rejoined onto a different root as if it were already relative,
// which would silently produce nonsense paths.
func TestCache_VersionBumpDiscardsSharedSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	mainRoot, otherRoot := gitModuleRepoWithWorktree(t)

	patterns := []string{"./..."}
	snap, err := Load(Options{Dir: mainRoot}, patterns...)
	if err != nil {
		t.Fatalf("Load(mainRoot): %v", err)
	}
	if err := SaveCache(mainRoot, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache(mainRoot): %v", err)
	}

	// Corrupt the on-disk version, simulating a cache written by an older
	// golance build before path relativization existed.
	file := CacheFile(mainRoot)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	patched := strings.Replace(string(data), `"version":5`, `"version":4`, 1)
	if patched == string(data) {
		t.Fatal("version field not found in cache JSON; test needs updating")
	}
	if err := os.WriteFile(file, []byte(patched), 0o600); err != nil {
		t.Fatalf("write patched cache file: %v", err)
	}

	if _, ok := LoadCache(otherRoot, patterns, nil); ok {
		t.Error("LoadCache(otherRoot) = ok for a cache written under an old version, want a miss")
	}
}
