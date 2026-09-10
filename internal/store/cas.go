package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// membersDirName is the CAS-directory-relative folder holding one marker
// file per index database that has ever recorded this directory as its own
// via DB.PutCASDir (see recordCASMember/CASMembers). internal/server's CAS
// GC reads it to build its candidate set for "every other database sharing
// this CAS" before ever trying to open any of them: an index database's own
// on-disk path is a deterministic function of its workspace root, not of
// its CAS directory (see internal/server's indexDBFile/casDir), so nothing
// about a database's path or filename can answer that question, and a
// database GC cannot open at all could otherwise never be attributed to a
// CAS by reading its content either.
const membersDirName = ".members"

// recordCASMember marks dbPath as an index database currently claiming dir
// as its own CAS directory, called by DB.PutCASDir right after it records
// the same fact inside dbPath's own database file. The marker file's name
// is dbPath's hash (bounded-length, collision-safe as a filename); its
// content is dbPath itself, so CASMembers can recover the original path
// without needing that hash to be reversible. Idempotent: recording the
// same (dir, dbPath) pair again just rewrites identical bytes. Marker files
// are never removed — a worktree whose database is later deleted leaves a
// harmless orphaned marker behind (see CASMembers' own doc for why that is
// an accepted tradeoff, not a correctness problem).
func recordCASMember(dir, dbPath string) error {
	membersDir := filepath.Join(dir, membersDirName)
	if err := os.MkdirAll(membersDir, 0o750); err != nil {
		return fmt.Errorf("store: create CAS members directory %s: %w", membersDir, err)
	}
	h := sha256.Sum256([]byte(dbPath))
	markerPath := filepath.Join(membersDir, fmt.Sprintf("%x", h[:16]))
	if err := os.WriteFile(markerPath, []byte(dbPath), 0o600); err != nil {
		return fmt.Errorf("store: write CAS member marker %s: %w", markerPath, err)
	}
	return nil
}

// CASMembers returns the on-disk path of every index database that has ever
// recorded dir as its own CAS directory via DB.PutCASDir, read directly
// from dir's own marker files (see recordCASMember) rather than by opening
// or globbing any index database file itself — the only way to attribute a
// database to dir before knowing whether that database can even be opened,
// so a caller like internal/server's CAS GC does not have to choose between
// treating every index database on the machine as a potential member of
// dir (risking an unrelated repository's database blocking this one) or
// giving up on attributing a database it cannot open at all (see (*CAS).GC's
// doc on why an incomplete mark set must defer the whole sweep).
//
// A returned path is a candidate, not a guarantee: it may no longer exist
// (its database file was since removed) or, in the unlikely event dbPath
// was reused for an unrelated root, may since have recorded a different
// CASDir. A caller that manages to open the path should still confirm via
// (*DB).CASDir before trusting its contents (see internal/server's
// collectOtherCASMarks).
func CASMembers(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, membersDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list CAS members %s: %w", dir, err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, membersDirName, e.Name())))
		if err != nil {
			continue
		}
		paths = append(paths, string(data))
	}
	return paths, nil
}

// blobPath returns the on-disk path for key, sharded two hex digits deep so
// no single directory ends up with one entry per package in a large
// workspace.
func (c *CAS) blobPath(key uint64) string {
	hex := fmt.Sprintf("%016x", key)
	return filepath.Join(c.dir, hex[:2], hex[2:]+".blob")
}

// Has reports whether key's blob exists. Callers that only need to confirm
// presence — e.g. a revalidation pass that can otherwise skip decoding —
// should prefer this over Get to avoid an unnecessary read.
func (c *CAS) Has(key uint64) bool {
	_, err := os.Stat(c.blobPath(key))
	return err == nil
}

// Get returns key's blob. ok is false if no blob is stored for key — always
// a soft miss, never an error on its own: a blob [CAS.GC] swept while it was
// still the current UnitPointer target for some database GC's mark set
// happened not to include (see GC's own safety doc) looks identical to one
// that was simply never computed, and the same recovery applies to both —
// the caller retypes-checks and re-Puts it, exactly like a GOCACHE miss.
// ctx is checked before the read starts, so a caller (e.g. internal/xref,
// mid a cancelable query) does not pay for a file read its own context has
// already given up on.
func (c *CAS) Get(ctx context.Context, key uint64) (blob []byte, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filepath.Clean(c.blobPath(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read CAS blob %x: %w", key, err)
	}
	return data, true, nil
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

// gcStampFile records the last time GC actually walked the CAS directory, so
// MaybeGC can bound how often that happens.
const gcStampFile = ".gc-stamp"

// Default GC constants: deliberately conservative and not flag-configurable
// for v0.1, mirroring GOCACHE's own unconfigurable defaults.
const (
	// GCInterval is how often MaybeGC actually walks the CAS directory when
	// called opportunistically (e.g. every server startup); calls in
	// between are a cheap timestamp-file stat and no-op. A schema-forced
	// rebuild calls GC directly instead, bypassing this throttle (see
	// internal/server's wiring), since that is precisely the rare event
	// that just orphaned a whole generation of blobs.
	GCInterval = 24 * time.Hour
	// GraceWindow is how recently an unreferenced blob may have been
	// written before GC will remove it. It exists for two reasons, not
	// one: (1) a blob can be Put before the UnitPointer update that
	// references it commits, so a GC pass racing that exact window must
	// not delete work a concurrent build only just produced; (2) this
	// GC's mark set is necessarily incomplete whenever another live
	// session's index database cannot be opened read-only within GC's own
	// short probe timeout (see GC's doc) — that session's own blobs may
	// simply not have been marked this round, not because they are
	// unreferenced. A blob only a few hours old is far more likely to be
	// "just written by a session GC could not read" than "orphaned by a
	// schema bump that happened days ago", so a wider grace window than a
	// bare race-condition guard would need trades a slower reclaim for
	// meaningfully lower risk of evicting something still live.
	GraceWindow = 48 * time.Hour
)

// GCStats summarizes one (*CAS).GC pass, for internal/server's log line
// (see its own doc for the exact format).
type GCStats struct {
	SweptCount int
	SweptBytes int64
	KeptCount  int
	KeptBytes  int64
	Duration   time.Duration
}

// GC removes every blob not present in marks and last written more than
// GraceWindow before now, reporting what it kept and swept. marks is the
// union of every UnitPointer.BlobKey currently recorded across every index
// database that shares this CAS directory (see internal/server's wiring,
// which builds it via (*DB).CollectBlobKeys across this session's own
// warm-opened database plus every other database CASMembers(dir) reports —
// see that function's own doc for why a candidate set built by opening or
// globbing every index database on the machine would not be safe here).
//
// Safety: a blob GC removes despite still being the correct target for some
// database's UnitPointer is not a correctness bug — [CAS.Get] already
// treats a missing blob as an ordinary cache miss, and the caller
// retypechecks and re-Puts it exactly as it would after a GOCACHE eviction
// (see Get's own doc). GC's mark set is therefore allowed to be an
// under-approximation (built best-effort, skipping any database it cannot
// read within a short timeout — see internal/server) without risking
// anything worse than an occasional avoidable recompute; GraceWindow exists
// to make that occasional case rare in practice, not to make it impossible.
// GC never reads a blob's own content (only os.Stat/os.Remove on the
// directory tree), so its own memory footprint stays flat regardless of how
// large the CAS directory is.
func (c *CAS) GC(now time.Time, marks map[uint64]struct{}) (GCStats, error) {
	return c.sweep(now, marks, GraceWindow)
}

// GCAged removes every blob last written more than maxAge before now,
// ignoring liveness entirely (marks is always nil) — for a CAS directory
// whose blobs are never referenced by any per-repo index database's
// UnitPointer at all, so GC's ordinary mark-and-sweep has no mark set it
// could ever build in the first place (internal/depexport's machine-global
// dependency export-data cache: nothing about which repository, worktree,
// or session asked for a dependency's export data is recorded anywhere a
// mark set could be derived from). A blob is always safely recomputable on
// a miss (see CAS.Get's doc), so an age-only sweep is exactly as safe here
// as GOCACHE's own unconditional age-based trim — maxAge simply plays
// GraceWindow's role, sized by the caller for a cache meant to survive far
// longer between actual reclaims than a per-repo one (see
// internal/depexport's own doc: "computed once per machine ever").
func (c *CAS) GCAged(now time.Time, maxAge time.Duration) (GCStats, error) {
	return c.sweep(now, nil, maxAge)
}

// sweep is GC and GCAged's shared directory walk, parameterized by grace —
// how recently an unmarked blob may have been written before it is
// removed — so each caller can supply its own tradeoff between reclaim
// speed and false-eviction risk (see GC's and GCAged's own docs) without
// duplicating the walk itself.
func (c *CAS) sweep(now time.Time, marks map[uint64]struct{}, grace time.Duration) (GCStats, error) {
	start := time.Now()
	var stats GCStats
	cutoff := now.Add(-grace)
	shards, err := os.ReadDir(c.dir)
	if err != nil {
		return stats, fmt.Errorf("store: list CAS directory: %w", err)
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
			key, ok := blobKeyFromFilename(shard.Name(), e.Name())
			if !ok {
				if isOrphanedPutTempFile(e.Name()) {
					sweepOrphanedTempFile(shardPath, e, cutoff, &stats)
				}
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			if _, referenced := marks[key]; referenced || fi.ModTime().After(cutoff) {
				stats.KeptCount++
				stats.KeptBytes += fi.Size()
				continue
			}
			size := fi.Size()
			if os.Remove(filepath.Join(shardPath, e.Name())) == nil {
				stats.SweptCount++
				stats.SweptBytes += size
			}
		}
	}
	stats.Duration = time.Since(start)
	return stats, nil
}

// isOrphanedPutTempFile reports whether name matches the "tmp-*.blob"
// pattern (*CAS).Put stages a write under, in the same shard directory,
// before renaming it into its final blobPath. blobKeyFromFilename never
// matches this shape (its 16 hex digits can never start with "tmp-"), so
// without this check a temp file left behind by a process that crashed
// between CreateTemp and Rename would never be swept at all, no matter how
// old — silently accumulating one leftover file per interrupted Put across
// the lifetime of a shared CAS directory.
func isOrphanedPutTempFile(name string) bool {
	return strings.HasPrefix(name, "tmp-") && strings.HasSuffix(name, ".blob")
}

// sweepOrphanedTempFile removes e — already known to match
// isOrphanedPutTempFile — if it was last written before cutoff, folding the
// reclaimed size into stats' swept counters exactly as an ordinary
// unreferenced blob would be. The same cutoff already governing every other
// removal in sweep applies here for the same reason: a temp file younger
// than the grace window may belong to a Put that is still in flight
// (CreateTemp has run; Rename has not yet), not one that crashed.
func sweepOrphanedTempFile(shardPath string, e os.DirEntry, cutoff time.Time, stats *GCStats) {
	fi, err := e.Info()
	if err != nil || fi.ModTime().After(cutoff) {
		return
	}
	size := fi.Size()
	if os.Remove(filepath.Join(shardPath, e.Name())) == nil {
		stats.SweptCount++
		stats.SweptBytes += size
	}
}

// MaybeGC runs GC(now, marks) only if at least GCInterval has passed since
// the last time it actually ran (tracked via a small stamp file in the CAS
// directory, shared by every worktree using this CAS — whichever session
// happens to observe the interval elapsed pays the walk for all of them).
// Safe to call on every session startup: the common case is a cheap stat of
// the stamp file followed by nothing. ran reports whether GC actually ran
// (false does not mean "nothing to report", it means "GC was skipped this
// time" — stats is zero either way).
func (c *CAS) MaybeGC(now time.Time, marks map[uint64]struct{}) (stats GCStats, ran bool, err error) {
	stampPath := filepath.Join(c.dir, gcStampFile)
	if fi, err := os.Stat(stampPath); err == nil && now.Sub(fi.ModTime()) < GCInterval {
		return GCStats{}, false, nil
	}
	// Claim the stamp first (best effort, not a lock): a lost race just
	// means two sessions both walk the directory once in the same window,
	// which is wasted work, not a correctness problem.
	if err := os.WriteFile(stampPath, nil, 0o600); err != nil {
		return GCStats{}, false, fmt.Errorf("store: write CAS GC stamp: %w", err)
	}
	stats, err = c.GC(now, marks)
	return stats, true, err
}

// MaybeGCAged runs GCAged(now, maxAge) only if at least interval has passed
// since the last time THIS METHOD actually ran (tracked via the same small
// stamp file MaybeGC uses — safe to share, since GCAged is only ever called
// against a CAS directory of c's own that MaybeGC never is, e.g.
// internal/depexport's machine-global directory, never a per-repo one).
// Mirrors MaybeGC's own throttle-and-run shape exactly, parameterized by
// maxAge/interval instead of the fixed GraceWindow/GCInterval constants, so
// a machine-global cache meant to be swept far less often, and with a far
// longer grace window, than a per-repo one can share this same mechanism
// rather than reimplementing it.
func (c *CAS) MaybeGCAged(now time.Time, maxAge, interval time.Duration) (stats GCStats, ran bool, err error) {
	stampPath := filepath.Join(c.dir, gcStampFile)
	if fi, err := os.Stat(stampPath); err == nil && now.Sub(fi.ModTime()) < interval {
		return GCStats{}, false, nil
	}
	if err := os.WriteFile(stampPath, nil, 0o600); err != nil {
		return GCStats{}, false, fmt.Errorf("store: write CAS GC stamp: %w", err)
	}
	stats, err = c.GCAged(now, maxAge)
	return stats, true, err
}

// blobKeyFromFilename recovers the uint64 key [CAS.blobPath] encoded into
// shardName/fileName (the inverse of blobPath's own "hex[:2]/hex[2:].blob"
// split), reporting ok=false for any directory entry that is not one of
// this CAS's own blob files (e.g. gcStampFile) rather than erroring — GC
// must tolerate whatever else has ever been written into this directory.
func blobKeyFromFilename(shardName, fileName string) (key uint64, ok bool) {
	hex, found := strings.CutSuffix(fileName, ".blob")
	if !found || len(shardName) != 2 || len(hex) != 14 {
		return 0, false
	}
	key, err := strconv.ParseUint(shardName+hex, 16, 64)
	if err != nil {
		return 0, false
	}
	return key, true
}
