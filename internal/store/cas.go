package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CAS is golance's content-addressed, lock-free blob store for one
// repository (see [OpenCAS]): every package "unit" — its facts blob, export
// data, and per-file stat snapshot for one exact source version — is stored
// as an immutable file named after its own content hash, the same principle
// GOCACHE uses for build artifacts. Two golance sessions (different
// worktrees, or the same worktree opened twice) writing the same content
// always compute the same key and so race harmlessly onto the same bytes:
// there is nothing to lock. See the package doc for the key's exact
// composition and why it must fold in a dependency's own key.
type CAS struct {
	dir string
}

// OpenCAS returns a CAS rooted at dir, creating dir if it does not exist.
// dir is typically <cache>/golance/cas-<repoKey> (see internal/server's
// repoKey): every worktree of a repository shares one CAS directory, since
// a blob's key already captures everything about its content — nothing
// about the reader's own root leaks into it (see Options.RelativePaths).
func OpenCAS(dir string) (*CAS, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create CAS directory %s: %w", dir, err)
	}
	return &CAS{dir: dir}, nil
}

// blobPath returns the on-disk path for key, sharded two hex digits deep so
// no single directory ends up with one entry per package in a large
// workspace.
func (c *CAS) blobPath(key uint64) string {
	hex := fmt.Sprintf("%016x", key)
	return filepath.Join(c.dir, hex[:2], hex[2:]+".blob")
}

// touchGranularity bounds how often Get/Has refreshes a blob's modification
// time: a blob already touched within this window is left alone, so a hot
// query loop does not turn every read into a write. Coarser than trimMaxAge
// by a wide margin, so "was this used recently enough to survive a trim"
// stays accurate regardless.
const touchGranularity = 24 * time.Hour

// Has reports whether key's blob exists, refreshing its last-used time (see
// [CAS.Get]'s doc) the same way a full read would. Callers that only need to
// confirm presence — e.g. a revalidation pass that can otherwise skip
// decoding — should prefer this over Get to avoid an unnecessary read.
func (c *CAS) Has(key uint64) bool {
	path := c.blobPath(key)
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	c.touch(path, fi)
	return true
}

// Get returns key's blob, refreshing its last-used time (mtime, at
// [touchGranularity] resolution) so a later [CAS.Trim] treats it as still in
// use. ok is false if no blob is stored for key. ctx is checked before the
// read starts, so a caller (e.g. internal/xref, mid a cancelable query)
// does not pay for a file read its own context has already given up on.
func (c *CAS) Get(ctx context.Context, key uint64) (blob []byte, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	path := c.blobPath(key)
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read CAS blob %x: %w", key, err)
	}
	if fi, statErr := os.Stat(path); statErr == nil {
		c.touch(path, fi)
	}
	return data, true, nil
}

// touch bumps path's mtime to now if it was last touched more than
// touchGranularity ago. Best effort: a failed Chtimes only risks an
// unnecessarily early Trim eviction for this one blob, never correctness
// (a package evicted while still needed is simply retyped-checked and
// rewritten, exactly like a GOCACHE miss).
func (c *CAS) touch(path string, fi os.FileInfo) {
	if time.Since(fi.ModTime()) < touchGranularity {
		return
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// Put stores blob under key, unless a blob is already stored there. Because
// the key is a deterministic function of blob's content (see the package
// doc), two writers computing the same key always have byte-identical
// content to write: Put stages the write in a temp file beside the target
// and renames it into place, so a concurrent writer for the same key either
// wins or loses the rename race harmlessly — both outcomes leave the exact
// same bytes at path. No locking is needed or used.
func (c *CAS) Put(key uint64, blob []byte) error {
	path := c.blobPath(key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("store: create CAS shard %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "tmp-*.blob")
	if err != nil {
		return fmt.Errorf("store: create CAS temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(blob)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		if writeErr != nil {
			return fmt.Errorf("store: write CAS blob %x: %w", key, writeErr)
		}
		return fmt.Errorf("store: close CAS temp file for %x: %w", key, closeErr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("store: rename CAS blob %x into place: %w", key, err)
	}
	return nil
}

// trimStampFile records the last time Trim ran, so MaybeTrim can bound how
// often a full directory walk happens.
const trimStampFile = ".trim-stamp"

// Default GC constants (see the package doc's "GC" design note): deliberately
// conservative and not flag-configurable for v0.1, mirroring GOCACHE's own
// unconfigurable defaults.
const (
	// TrimInterval is how often MaybeTrim actually walks the CAS directory;
	// calls in between are a cheap timestamp-file stat and no-op.
	TrimInterval = 24 * time.Hour
	// TrimMaxAge is how long a blob may go unused (see touchGranularity)
	// before Trim removes it.
	TrimMaxAge = 5 * 24 * time.Hour
)

// MaybeTrim runs Trim(TrimMaxAge) only if at least TrimInterval has passed
// since the last time it actually ran (tracked via a small stamp file in the
// CAS directory, shared by every worktree using this CAS — whichever session
// happens to observe the interval elapsed pays the walk for all of them).
// Safe to call on every session startup: the common case is a cheap stat of
// the stamp file followed by nothing.
func (c *CAS) MaybeTrim(now time.Time) error {
	stampPath := filepath.Join(c.dir, trimStampFile)
	if fi, err := os.Stat(stampPath); err == nil && now.Sub(fi.ModTime()) < TrimInterval {
		return nil
	}
	// Claim the stamp first (best effort, not a lock): a lost race just
	// means two sessions both walk the directory once in the same window,
	// which is wasted work, not a correctness problem.
	if err := os.WriteFile(stampPath, nil, 0o600); err != nil {
		return fmt.Errorf("store: write CAS trim stamp: %w", err)
	}
	_, err := c.Trim(now, TrimMaxAge)
	return err
}

// Trim removes every blob whose last-used mtime is older than now.Add(-maxAge),
// reporting how many were removed. Blobs read or written more recently than
// that (including via a mere [CAS.Has] presence check — see its doc) survive:
// a currently in-use blob is always touched at least once per golance
// session that references it, so only a package genuinely unvisited for
// maxAge is ever collected.
func (c *CAS) Trim(now time.Time, maxAge time.Duration) (removed int, err error) {
	cutoff := now.Add(-maxAge)
	shards, err := os.ReadDir(c.dir)
	if err != nil {
		return 0, fmt.Errorf("store: list CAS directory: %w", err)
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(c.dir, shard.Name())
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(shardPath, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed, nil
}
