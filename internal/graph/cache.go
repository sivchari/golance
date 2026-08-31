package graph

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// diskCache is the on-disk JSON envelope for a persisted Snapshot. Patterns
// and BuildFlags are folded into the cache key: LoadCache refuses to serve
// a cache built for a different pattern set or BuildFlags, since either
// changes what packages.Load would compute.
//
// TODO(v0.1): //go:embed footprint tracking (gopls-lazy graph.go's
// embedFiles/embedPrefixes) is not ported; a non-Go file change under an
// embed pattern will not invalidate the cache.
type diskCache struct {
	Root       string              `json:"root"`
	Patterns   []string            `json:"patterns"`
	BuildFlags []string            `json:"buildFlags,omitempty"`
	Packages   map[string]*Package `json:"packages"`
	// ModuleFiles records, keyed by path, which of moduleFiles(root)
	// existed when this cache was saved — see Stale's doc for why this is
	// needed to detect one of those files being deleted, not just
	// modified. Absent (a cache written before this field existed) is
	// treated as "none existed": such a cache simply cannot detect a
	// deletion that happened before its own next rebuild, rather than
	// producing a false positive.
	ModuleFiles map[string]bool `json:"moduleFiles,omitempty"`
}

// CacheFile returns the on-disk cache path for a workspace root, under
// $XDG_CACHE_HOME (or the platform default via os.UserCacheDir).
func CacheFile(root string) string {
	h := sha256.Sum256([]byte(root))
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(base, "golance", fmt.Sprintf("graph-%x.json", h[:8]))
}

// LoadCache reads a Snapshot previously persisted by SaveCache for root. ok
// is false when there is no cache file, it is corrupt, or it was built for
// different patterns or BuildFlags than requested.
func LoadCache(root string, patterns, buildFlags []string) (snap *Snapshot, ok bool) {
	data, err := os.ReadFile(filepath.Clean(CacheFile(root)))
	if err != nil {
		return nil, false
	}
	var saved diskCache
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, false
	}
	if saved.Root != root || !equalStrings(saved.Patterns, patterns) || !equalStrings(saved.BuildFlags, buildFlags) {
		return nil, false
	}
	snap, err = newSnapshot(saved.Packages, saved.Root, saved.BuildFlags)
	if err != nil {
		return nil, false
	}
	return snap, true
}

// SaveCache persists snap to disk for a later LoadCache(root, patterns,
// buildFlags) call.
func SaveCache(root string, patterns, buildFlags []string, snap *Snapshot) error {
	files := moduleFiles(root)
	existed := make(map[string]bool, len(files))
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			existed[f] = true
		}
	}
	saved := diskCache{Root: root, Patterns: patterns, BuildFlags: buildFlags, Packages: snap.Packages, ModuleFiles: existed}
	b, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("graph: marshal cache: %w", err)
	}
	file := CacheFile(root)
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return fmt.Errorf("graph: create cache dir: %w", err)
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("graph: write cache: %w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("graph: rename cache: %w", err)
	}
	return nil
}

// Stale reports whether the on-disk cache for root should be rebuilt: true
// when there is no cache, when go.mod/go.sum/go.work/go.work.sum is newer
// than the cache file, or when one of those files that existed when the
// cache was saved has since been deleted (e.g. a `git checkout` that drops
// go.work) — a missing file trivially fails os.Stat and carries no ModTime
// to compare, so its removal would otherwise never be observed. Any other
// file change (source, non-Go assets) never makes the graph stale, since it
// cannot change the import graph.
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
			if existedAtSave[f] {
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
