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
	data, err := os.ReadFile(CacheFile(root))
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
	saved := diskCache{Root: root, Patterns: patterns, BuildFlags: buildFlags, Packages: snap.Packages}
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
// when there is no cache, or when go.mod/go.sum/go.work/go.work.sum is
// newer than the cache file. Any other file change (source, non-Go assets)
// never makes the graph stale, since it cannot change the import graph.
func Stale(root string) bool {
	fi, err := os.Stat(CacheFile(root))
	if err != nil {
		return true
	}
	cacheT := fi.ModTime()
	for _, f := range moduleFiles(root) {
		if s, err := os.Stat(f); err == nil && !s.ModTime().Before(cacheT) {
			return true
		}
	}
	return false
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
