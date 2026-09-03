package graph

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cacheVersion guards against loading a diskCache written by an older
// golance build whose Package shape is missing a field a newer one now
// relies on — in particular Package.Name, added for unimported-completion
// candidates: LoadCache from an old cache would otherwise silently succeed
// with every package's Name empty (a zero-value JSON field is
// indistinguishable from "actually blank"), leaving unimported completion
// quietly broken until something else happens to trigger a full reload
// (see Stale, which only watches go.mod/go.sum/go.work — a Name-less cache
// is not "stale" by that definition). Bump this whenever diskCache's shape
// changes in a way an old cache cannot satisfy.
//
// v3: Load now requests test variants (Config.Tests, see Load's doc), so an
// old cache's Packages map is missing every test-only dependency (e.g.
// "testing", "testify") entirely, and every Package.TestImports is
// zero-valued rather than genuinely empty — the same
// indistinguishable-from-blank problem Package.Name had, now for the whole
// graph completeness fix this version exists to deliver.
//
// v4: CacheFile is now keyed by RepoKey(root) rather than root itself, so
// every worktree of one git repository shares a single cache file (see
// CacheFile's doc), and every stored Package.Dir/GoFiles/ExportFile that
// falls under the SAVING worktree's own root is written relative to it
// (relPath) and rejoined onto the LOADING (current) root at LoadCache time
// (absPath) — see toDiskPackages/fromDiskPackages. A pre-v4 cache's paths
// are plain absolute strings for whichever single worktree wrote them and
// would resolve to nonsense joined onto a different root, so it cannot be
// reused here even though the JSON shape is otherwise compatible.
//
// v5: Package.ExportFile is gone (see internal/depexport's package doc for
// what replaced `go list -export` entirely); a pre-v5 cache's Packages map
// still carries populated ExportFile strings that would silently decode
// into a JSON field nothing reads anymore. Harmless on its own, but bumped
// anyway so an old, ExportFile-shaped cache is never treated as
// interchangeable with a new one purely by coincidence of an unrelated
// field being ignored.
const cacheVersion = 5

// diskCache is the on-disk JSON envelope for a persisted Snapshot. Patterns
// and BuildFlags are folded into the cache key: LoadCache refuses to serve
// a cache built for a different pattern set or BuildFlags, since either
// changes what packages.Load would compute.
//
// TODO(v0.1): //go:embed footprint tracking (gopls-lazy graph.go's
// embedFiles/embedPrefixes) is not ported; a non-Go file change under an
// embed pattern will not invalidate the cache.
type diskCache struct {
	Version    int                 `json:"version"`
	Patterns   []string            `json:"patterns"`
	BuildFlags []string            `json:"buildFlags,omitempty"`
	Packages   map[string]*Package `json:"packages"`
	// ModuleFiles records, keyed by path RELATIVE to the saving worktree's
	// own root (mirroring Packages' own relPath treatment — see
	// cacheVersion's v4 note), which of moduleFiles(root) existed when this
	// cache was saved — see Stale's doc for why this is needed to detect one
	// of those files being deleted, not just modified. Absent (a cache
	// written before this field existed) is treated as "none existed": such
	// a cache simply cannot detect a deletion that happened before its own
	// next rebuild, rather than producing a false positive.
	ModuleFiles map[string]bool `json:"moduleFiles,omitempty"`
}

// RepoKey returns the identity graph's cache uses to decide whether root's
// on-disk graph cache (see CacheFile) is shared with other worktrees:
// the absolute, symlink-resolved path of `git rev-parse --git-common-dir`
// run in root, when root is inside a git repository — every worktree of the
// same repository shares this one key, since --git-common-dir always
// resolves to the same directory regardless of which worktree it is run
// from — or root itself, with shared=false, otherwise (a plain non-git
// workspace, which keeps its own private cache exactly as golance behaved
// before worktree sharing existed; there is nothing to share, and no
// benefit to paying the relative-path bookkeeping for it).
//
// Mirrors internal/server's own (private) repoKey 1:1 — that package wraps
// this one rather than keeping an independent implementation, so the CAS,
// facts index, and graph cache can never disagree about which worktrees
// share what.
func RepoKey(root string) (key string, shared bool) {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return root, false
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return root, false
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return root, false
	}
	// Resolve symlinks so every worktree's key lands on the same string
	// regardless of which absolute form its own root happened to be given
	// in — see internal/server's identical repoKey for the macOS
	// /var -> /private/var example this guards against.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, true
}

// Shared reports whether root's graph cache (see CacheFile) is shared with
// other worktrees of the same git repository.
func Shared(root string) bool {
	_, shared := RepoKey(root)
	return shared
}

// CacheFile returns the on-disk cache path for a workspace root, under
// $XDG_CACHE_HOME (or the platform default via os.UserCacheDir). Keyed by
// RepoKey(root), not root itself: every worktree of the same git repository
// resolves to the same path, so a graph cache one worktree already paid to
// build (see loadMode's doc for that cost on a large monorepo) is directly
// available to a brand-new sibling worktree instead of that worktree
// needing its own cold `go list`. A non-git root's RepoKey is root itself
// (shared=false), so this collapses back to golance's pre-sharing,
// private-per-root behavior for that case — the same function, no special
// casing needed.
func CacheFile(root string) string {
	key, _ := RepoKey(root)
	h := sha256.Sum256([]byte(key))
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(base, "golance", fmt.Sprintf("graph-%x.json", h[:8]))
}

// LoadCache reads a Snapshot previously persisted by SaveCache, rejoined
// onto root: every stored package path that SaveCache made relative to
// ITS OWN (possibly different) worktree root is joined back onto root here
// (see fromDiskPackages/absPath), so a cache another worktree of the same
// repository saved loads correctly for this one. ok is false when there is
// no cache file, it is corrupt, or it was built for different patterns or
// BuildFlags than requested.
func LoadCache(root string, patterns, buildFlags []string) (snap *Snapshot, ok bool) {
	data, err := os.ReadFile(filepath.Clean(CacheFile(root)))
	if err != nil {
		return nil, false
	}
	var saved diskCache
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, false
	}
	if saved.Version != cacheVersion || !equalStrings(saved.Patterns, patterns) || !equalStrings(saved.BuildFlags, buildFlags) {
		return nil, false
	}
	snap, err = newSnapshot(fromDiskPackages(saved.Packages, root), root)
	if err != nil {
		return nil, false
	}
	return snap, true
}

// SaveCache persists snap to disk for a later LoadCache call, from this or
// any other worktree of the same repository (see CacheFile). Every package
// path under root is stored relative to it (see toDiskPackages); a
// dependency path outside root (GOROOT, module cache — already stable
// across worktrees) is stored exactly as-is.
//
// Because CacheFile is shared across every worktree of a repository (see
// its own doc), two SaveCache calls for different worktrees — e.g. the
// interactive server's background revalidateGraph and a concurrently
// running indexer subprocess — can race to write it at once. Each call
// stages its write in its OWN uniquely-named temp file (os.CreateTemp, not
// a fixed "<file>.tmp" name) before renaming it into place, so the two
// renames never collide: whichever finishes last simply wins, and the
// other's rename still succeeds against a temp file only it ever touched,
// instead of failing with ENOENT because a sibling caller already renamed
// (and thereby removed) the shared fixed name out from under it.
func SaveCache(root string, patterns, buildFlags []string, snap *Snapshot) error {
	files := moduleFiles(root)
	existed := make(map[string]bool, len(files))
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			existed[relPath(root, f)] = true
		}
	}
	saved := diskCache{
		Version:     cacheVersion,
		Patterns:    patterns,
		BuildFlags:  buildFlags,
		Packages:    toDiskPackages(snap.Packages, root),
		ModuleFiles: existed,
	}
	b, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("graph: marshal cache: %w", err)
	}
	file := CacheFile(root)
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("graph: create cache dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(file)+".tmp-*")
	if err != nil {
		return fmt.Errorf("graph: create cache temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(b)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		if writeErr != nil {
			return fmt.Errorf("graph: write cache: %w", writeErr)
		}
		return fmt.Errorf("graph: close cache temp file: %w", closeErr)
	}
	if err := os.Rename(tmpPath, file); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("graph: rename cache: %w", err)
	}
	return nil
}

// toDiskPackages returns a copy of pkgs whose Dir/GoFiles are made relative
// to root wherever they fall under it (relPath) — the transform SaveCache
// applies before marshaling, so the JSON on disk never embeds an absolute
// path specific to this one worktree for anything worktree-local. pkgs
// itself is never mutated: it may be the live snap.Packages map,
// concurrently read by other request handlers.
func toDiskPackages(pkgs map[string]*Package, root string) map[string]*Package {
	out := make(map[string]*Package, len(pkgs))
	for path, pkg := range pkgs {
		cp := *pkg
		cp.Dir = relPath(root, pkg.Dir)
		cp.GoFiles = relFiles(root, pkg.GoFiles)
		out[path] = &cp
	}
	return out
}

// fromDiskPackages reverses toDiskPackages, joining every stored path back
// onto root (absPath) — the transform LoadCache applies right after
// unmarshaling, so every consumer of the resulting Snapshot keeps seeing
// real absolute paths exactly as a freshly-Loaded one would, regardless of
// which worktree originally saved this cache.
func fromDiskPackages(pkgs map[string]*Package, root string) map[string]*Package {
	out := make(map[string]*Package, len(pkgs))
	for path, pkg := range pkgs {
		cp := *pkg
		cp.Dir = absPath(root, pkg.Dir)
		cp.GoFiles = absFiles(root, pkg.GoFiles)
		out[path] = &cp
	}
	return out
}

// relPath returns path relative to root when path falls under it, or path
// itself unchanged otherwise (e.g. a GOROOT or module-cache dependency,
// already stable across worktrees — see toDiskPackages' doc) — mirroring
// internal/index and internal/xref's own identical relPath helpers, kept as
// graph's own small copy rather than a shared dependency since none of
// these packages otherwise depend on each other.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// absPath reverses relPath: it joins stored back onto root, unless stored
// is already absolute (a dependency path relPath left untouched, or a
// pre-relativization on-disk value).
func absPath(root, stored string) string {
	if filepath.IsAbs(stored) {
		return stored
	}
	return filepath.Join(root, stored)
}

func relFiles(root string, files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = relPath(root, f)
	}
	return out
}

func absFiles(root string, files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = absPath(root, f)
	}
	return out
}

// Stale reports whether the on-disk cache for root should be rebuilt: true
// when there is no cache, when go.mod/go.sum/go.work/go.work.sum is newer
// than the cache file, or when one of those files that existed when the
// cache was saved has since been deleted (e.g. a `git checkout` that drops
// go.work) — a missing file trivially fails os.Stat and carries no ModTime
// to compare, so its removal would otherwise never be observed. Any other
// file change (source, non-Go assets) never makes the graph stale, since it
// cannot change the import graph.
//
// For a SHARED cache (see CacheFile/Shared), this mtime-based check alone
// is not a reliable enough staleness signal on its own — the cache file's
// mtime reflects whenever any worktree of the repository last saved it, and
// two worktrees can be on different branches with identical go.mod/go.sum
// but a genuinely different file set — so callers loading a shared cache
// should always kick a background revalidation (graph.Shared(root)) in
// addition to checking Stale, rather than relying on Stale alone the way a
// private, non-shared cache safely can.
func Stale(root string) bool {
	cacheFile := CacheFile(root)
	fi, err := os.Stat(cacheFile)
	if err != nil {
		return true
	}
	cacheT := fi.ModTime()
	existedAtSave := moduleFilesExistedAtSave(cacheFile)
	for _, f := range moduleFiles(root) {
		s, err := os.Stat(f)
		if err != nil {
			if existedAtSave[relPath(root, f)] {
				return true
			}
			continue
		}
		if !s.ModTime().Before(cacheT) {
			return true
		}
	}
	return false
}

// moduleFilesExistedAtSave returns the ModuleFiles set recorded in
// cacheFile's diskCache, or nil if the cache is missing, corrupt, or was
// written before that field existed (see diskCache.ModuleFiles's doc).
func moduleFilesExistedAtSave(cacheFile string) map[string]bool {
	data, err := os.ReadFile(filepath.Clean(cacheFile))
	if err != nil {
		return nil
	}
	var saved diskCache
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil
	}
	return saved.ModuleFiles
}

// moduleFiles lists the module-structural files whose modification implies
// the dependency graph may have changed.
//
// TODO(v0.2): gopls-lazy's moduleFiles also walks go.work `use` directives
// to cover nested modules; v0.1 is single-module only (see plan-feat-v0.1.md).
func moduleFiles(root string) []string {
	return []string{
		filepath.Join(root, "go.mod"),
		filepath.Join(root, "go.sum"),
		filepath.Join(root, "go.work"),
		filepath.Join(root, "go.work.sum"),
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
