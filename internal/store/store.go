package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// ErrNotFound is returned by Get-style lookups that find no entry for the
// given key.
var ErrNotFound = errors.New("store: not found")

// openTimeout bounds how long Open waits to acquire bbolt's exclusive file
// lock before giving up. Without it, bbolt.Open retries indefinitely and
// never returns an error, so a second golance session opening the same
// workspace root (e.g. the same folder in a second editor window) would
// hang its "initialize" request forever instead of failing fast — see
// [DB]'s doc.
const openTimeout = 5 * time.Second

var (
	bucketUnit               = []byte("unit")            // pkgPathHash -> UnitPointer (see PutUnit)
	bucketMeta               = []byte("meta")            // reserved keys, e.g. buildFingerprintKey
	bucketName               = []byte("name")            // lowercased symbol name -> []symbolIDHash
	bucketMethod             = []byte("method")          // method name -> []MethodEntry
	bucketSymStr             = []byte("symstr")          // symbolIDHash -> []original SymbolID string
	bucketRefPostings        = []byte("refposting")      // (targetPkgHash, targetIDHash, srcPkgHash) -> []PostingLocation (see PostingsFor)
	bucketRefPostingManifest = []byte("refpostmanifest") // srcPkgHash -> [](targetPkgHash, targetIDHash), for exact incremental delete (see applyPostings)
)

var allBuckets = [][]byte{bucketUnit, bucketMeta, bucketName, bucketMethod, bucketSymStr, bucketRefPostings, bucketRefPostingManifest}

// buildFingerprintKey is a reserved bucketMeta key for the whole
// database's build fingerprint (see PutBuildFingerprint).
var buildFingerprintKey = []byte("\x00golance:fingerprint")

// schemaVersion is the on-disk layout version of the unit/name/method/
// symstr/refposting/refpostmanifest buckets (see UnitPointer's encoding in
// encodeUnitPointer). Bump it whenever that encoding changes incompatibly;
// Open discards and recreates any database it finds recorded under a
// different version, or under none at all (see discardStale's doc) — the
// same byte layout has been used since before this check existed, so a
// stale database's "unit" records decode without error; they are just
// silently wrong once the [CAS] blobs their BlobKey fields point at are no
// longer the ones that were current when those records were written (e.g.
// after a schema change, or a CAS directory that was never populated from
// the same build that wrote this database — see casDir's doc in
// internal/server). A decode-error check alone cannot catch that; this
// version marker can.
//
// Bumped to 3 for the reverse reference index (bucketRefPostings/
// bucketRefPostingManifest, see applyPostings/PostingsFor): a database that
// predates this change has neither bucket at all, so References would
// silently return zero results for every symbol instead of erroring — the
// same "wrong, not merely missing" failure mode every other schemaVersion
// bump here exists to force a rebuild past. See internal/index's
// factsSchemaVersion for the matching CAS-side key bump (a discarded
// database's fresh Open still needs freshly extracted PostingEntry data,
// not just fresh buckets to put it in).
//
// Bumped to 2 for [MethodEntry]'s three new fields (MethodPkgHash,
// MethodIDHash, Fingerprint — see its doc): methodEntrySize grew from 16 to
// 40 bytes, so a version-1 "method" bucket's posting lists would otherwise
// misdecode under the new fixed stride instead of erroring outright. See
// internal/index's factsSchemaVersion for the matching CAS-side key bump
// (this field alone does not force a rebuild of the [CAS] blobs a discarded
// database's fresh Open would otherwise point right back at).
const schemaVersion uint16 = 3

// schemaVersionKey is a reserved bucketMeta key recording the schemaVersion
// the database's buckets were last (re)written under.
var schemaVersionKey = []byte("\x00golance:schema")

// DB is a handle to golance's per-root index: a small bbolt database mapping
// each workspace package to its current [CAS] blob key (see [UnitPointer]),
// plus the name/method/SymbolID-string lookup indices built from those
// blobs' contents. It holds no facts or export data itself — those live in
// the shared, lock-free [CAS] this root's session opens alongside it (see
// the package doc). A *DB is safe for concurrent use by multiple
// goroutines, and — being per-root/per-worktree rather than shared across a
// whole repository — is not contended across different roots the way the
// old single shared bbolt database was. Two golance sessions opening the
// same root at once still contend on this file's lock; see [Open].
type DB struct {
	bolt *bbolt.DB
	// path is the on-disk file Open/OpenReadOnly opened this handle from,
	// kept so Compact can replace it in place without every caller having
	// to pass it back in.
	path string
	// recreated records whether THIS Open call found path already written
	// under an older/missing schemaVersion and discarded it (see
	// discardStale): true only for the Open call that actually performed
	// the discard-and-recreate, never for a subsequent Open of the fresh
	// file. internal/server's GC wiring uses WasRecreated to decide whether
	// a just-finished build is the rare "full rebuild forced by a schema
	// bump" case Compact should run for, as opposed to an ordinary
	// incremental reindex of an already-current database.
	recreated bool
	// readOnly records whether this handle came from OpenReadOnly: Compact
	// replaces the underlying file, which only the exclusive writer this
	// handle represents may safely do.
	readOnly bool
}

// Open opens (creating if necessary) the index database at path and ensures
// all buckets exist. Unlike the pre-CAS design, this is always a per-root
// file: nothing about it is ever shared across worktrees. It is still an
// exclusively-locked OS file, though, so opening the same root's database
// from two golance sessions at once (e.g. the same workspace open in a
// second editor window) contends on that lock; Open waits up to openTimeout
// for it before giving up, rather than blocking its caller forever.
//
// A database found at path but recorded under a different schemaVersion (or
// none at all — see discardStale) is discarded and recreated empty rather
// than opened as-is: it is a regenerable cache, not user data, and the
// caller's usual "nothing indexed yet" recovery path (index.Revalidate
// reports every package as not-yet-in-db, triggering a full rebuild — see
// internal/server.indexNeedsRebuild) already handles an empty database
// correctly.
func Open(path string) (*DB, error) {
	bdb, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, wrapOpenErr(path, err)
	}
	var recreated bool
	if discardStale(bdb) {
		recreated = true
		if err := bdb.Close(); err != nil {
			return nil, fmt.Errorf("store: close stale %s: %w", path, err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("store: remove stale %s: %w", path, err)
		}
		bdb, err = bbolt.Open(path, 0o600, &bbolt.Options{Timeout: openTimeout})
		if err != nil {
			return nil, wrapOpenErr(path, err)
		}
	}
	err = bdb.Update(func(tx *bbolt.Tx) error {
		for _, b := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketMeta).Put(schemaVersionKey, schemaVersionBytes())
	})
	if err != nil {
		_ = bdb.Close()
		return nil, fmt.Errorf("store: init buckets: %w", err)
	}
	return &DB{bolt: bdb, path: path, recreated: recreated}, nil
}

// OpenReadOnly opens an already-initialized index database at path without
// acquiring bbolt's exclusive write lock: reads (GetUnit, LookupMethod,
// etc.) behave exactly as with [Open], but every write
// (PutUnit/PutUnitsBatch/PutUnitPointersBatch/PutBuildFingerprint) fails
// immediately with bbolt's ErrDatabaseReadOnly instead of persisting.
// Unlike Open, it never creates the file or its buckets and never discards
// a stale schema — path must already have been through a successful Open.
// Test-only: production code always uses [Open].
func OpenReadOnly(path string) (*DB, error) {
	return openReadOnlyTimeout(path, openTimeout)
}

// OpenReadOnlyTimeout is [OpenReadOnly] with an explicit lock-wait timeout,
// for a caller that must not block for openTimeout's full 5 seconds — e.g.
// internal/server's CAS GC, probing an unbounded number of other sessions'
// index databases and needing to move on immediately from one currently
// held open by a live writer (see (*CAS).GC's doc) rather than wait it out.
func OpenReadOnlyTimeout(path string, timeout time.Duration) (*DB, error) {
	return openReadOnlyTimeout(path, timeout)
}

func openReadOnlyTimeout(path string, timeout time.Duration) (*DB, error) {
	bdb, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: timeout, ReadOnly: true})
	if err != nil {
		return nil, wrapOpenErr(path, err)
	}
	return &DB{bolt: bdb, path: path, readOnly: true}, nil
}

// wrapOpenErr wraps a bbolt.Open failure for path with context, calling out
// the lock-timeout case specifically (errors.Is(err, bolterrors.ErrTimeout) still
// holds through the %w) so a caller's log line explains what actually
// happened instead of reading like an opaque file error.
func wrapOpenErr(path string, err error) error {
	if errors.Is(err, bolterrors.ErrTimeout) {
		return fmt.Errorf("store: open %s: locked by another golance session for over %s: %w", path, openTimeout, err)
	}
	return fmt.Errorf("store: open %s: %w", path, err)
}

// IsLocked reports whether err — an error returned by [Open] or
// [OpenReadOnly] — means path is currently held open by another live
// process (bbolt's ErrTimeout, wrapped by wrapOpenErr), as opposed to some
// other failure (e.g. a missing parent directory or a corrupt file).
// internal/server uses this to distinguish "another golance session
// already has this database open" — worth falling back to a different
// path for — from an ordinary open failure, which is not.
func IsLocked(err error) bool {
	return errors.Is(err, bolterrors.ErrTimeout)
}

// probeTimeout bounds how long TryClaimAbandoned waits for path's lock
// before concluding it is genuinely held by another process. Much shorter
// than openTimeout: TryClaimAbandoned is a background "is anyone still
// using this" probe across many candidate files, not a real open a caller
// is blocked on, so it should fail fast on a file that is actually still
// in use.
const probeTimeout = 20 * time.Millisecond

// TryClaimAbandoned attempts to open path with a short lock-wait timeout
// and, if that succeeds — meaning no other process currently holds path's
// exclusive lock — closes it immediately and removes the file, reporting
// true. It reports false, leaving path untouched, if the lock is currently
// held by another process (the file is still genuinely in use) or any
// other error occurs (e.g. path no longer exists, already removed by a
// racing cleanup pass in another session started at the same time — see
// internal/server's startup orphan cleanup). Never creates path if it does
// not already exist.
func TryClaimAbandoned(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	bdb, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: probeTimeout})
	if err != nil {
		return false
	}
	if err := bdb.Close(); err != nil {
		return false
	}
	return os.Remove(path) == nil
}

// discardStale reports whether bdb's meta bucket already exists (meaning
// some earlier Open — old or new code — has run against this file at least
// once) but does not record the current schemaVersion. A brand new file has
// no meta bucket yet at all, since nothing has called
// CreateBucketIfNotExists on it; that case is never stale, as there is
// nothing in it to distrust.
func discardStale(bdb *bbolt.DB) bool {
	var stale bool
	_ = bdb.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return nil
		}
		v := meta.Get(schemaVersionKey)
		stale = len(v) != 2 || binary.LittleEndian.Uint16(v) != schemaVersion
		return nil
	})
	return stale
}

func schemaVersionBytes() []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, schemaVersion)
	return b
}

// Close closes the underlying database file.
func (db *DB) Close() error { return db.bolt.Close() }

// WasRecreated reports whether this Open call found path already written
// under a stale schemaVersion (or none at all) and discarded it, per
// discardStale's doc. False for a database that was already current, and
// false for a brand new file (nothing existed to distrust — see
// discardStale). See the recreated field's own doc for how internal/server
// uses this to decide when to run a schema-rebuild-triggered Compact.
func (db *DB) WasRecreated() bool { return db.recreated }

// casDirKey is a reserved bucketMeta key recording the [CAS] directory this
// database's UnitPointer.BlobKey values resolve against (see PutCASDir).
var casDirKey = []byte("\x00golance:casdir")

// PutCASDir records dir as this database's owning CAS directory, overwriting
// any previous value. internal/server calls this once right after every
// successful Open, alongside the CAS directory it always opens in lockstep
// (see internal/server's casDir/tryWarmOpen/openIndexAfterBuild): unlike
// indexDBFile, which is keyed by root and so never shared across worktrees,
// casDir is keyed by repository identity and IS shared — there is no way to
// invert an index database's own filename hash back to the CAS directory it
// was built against, so a CAS GC pass that needs to enumerate every
// database sharing one CAS directory (see (*CAS).GC's doc) reads this
// meta value back out of each database file it finds instead.
func (db *DB) PutCASDir(dir string) error {
	return db.bolt.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(casDirKey, []byte(dir))
	})
}

// CASDir returns the CAS directory last recorded via PutCASDir. It returns
// ErrNotFound if none has been recorded yet — either a database written
// before this feature existed, or one PutCASDir was never called for.
func (db *DB) CASDir() (string, error) {
	var dir string
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketMeta).Get(casDirKey)
		if v == nil {
			return ErrNotFound
		}
		dir = string(v)
		return nil
	})
	return dir, err
}

// CollectBlobKeys adds every UnitPointer.BlobKey currently recorded in db to
// marks (a set, keyed by BlobKey; the value is unused). It streams the
// "unit" bucket via a cursor rather than materializing every UnitPointer at
// once, so a CAS GC pass building a mark set across many databases (see
// (*CAS).GC's doc) never holds more than one record's worth of decoded data
// at a time — only the resulting uint64 keys accumulate in marks, never any
// blob content. A record that fails to decode (a corrupt or foreign-schema
// entry, which discardStale/schemaVersion should already rule out in
// practice) is skipped rather than aborting the whole scan: GC's own
// correctness never depends on a complete mark set (see the package doc's
// "CAS garbage collection" section), only on not sweeping something it
// easily could have known was still referenced.
func (db *DB) CollectBlobKeys(marks map[uint64]struct{}) error {
	return db.bolt.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketUnit).Cursor()
		for _, v := c.First(); v != nil; _, v = c.Next() {
			p, err := decodeUnitPointer(v)
			if err != nil {
				continue
			}
			marks[p.BlobKey] = struct{}{}
		}
		return nil
	})
}

// Compact rewrites db's underlying bbolt file into a fresh one via
// bbolt.Compact — reclaiming the free-list space a schema-wipe rebuild
// leaves behind (bbolt only reuses freed pages within the same file; it
// never shrinks the file itself) — then atomically replaces the original
// with it and reopens db.bolt against the new file. Only valid on
// a writable handle (from Open): the whole point is to replace the file
// this handle's own OS lock is protecting, which a read-only handle has no
// business doing. Callers should reserve this for the rare schema-forced
// full rebuild (see WasRecreated), not every ordinary reindex — Compact
// re-reads and rewrites the entire database, which is wasted work for a
// database that was already reasonably sized.
//
// On any failure past the point where the original file has been closed,
// Compact still leaves db.bolt reopened against db.path (the original,
// untouched database if the rename step itself failed; the successfully
// compacted one otherwise) before returning its error, so a caller that
// only logs Compact's failure and carries on (see cmd/golance's own
// caller) is never left holding a permanently unusable handle.
func (db *DB) Compact() error {
	if db.readOnly {
		return errors.New("store: Compact called on a read-only database handle")
	}
	tmpPath := db.path + ".compact-tmp"
	dst, err := bbolt.Open(tmpPath, 0o600, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return fmt.Errorf("store: compact %s: open temp file: %w", db.path, err)
	}
	if err := bbolt.Compact(dst, db.bolt, 0); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("store: compact %s: %w", db.path, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("store: compact %s: close temp file: %w", db.path, err)
	}
	if err := db.bolt.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("store: compact %s: close original before replacing: %w", db.path, err)
	}

	renameErr := os.Rename(tmpPath, db.path)
	if renameErr != nil {
		_ = os.Remove(tmpPath)
	}
	// db.path now names a valid database file either way: the freshly
	// compacted one on a successful rename, or — os.Rename either fully
	// succeeds or leaves its destination untouched, never partially — the
	// original, un-compacted one if the rename itself failed. Reopen
	// whichever it is so a caller that only logs Compact's error (see
	// cmd/golance's own caller) is left with a usable handle instead of a
	// permanently closed one.
	reopened, reopenErr := bbolt.Open(db.path, 0o600, &bbolt.Options{Timeout: openTimeout})
	if reopenErr != nil {
		return fmt.Errorf("store: compact %s: reopen after compaction: %w", db.path, reopenErr)
	}
	db.bolt = reopened
	if renameErr != nil {
		return fmt.Errorf("store: compact %s: rename temp file into place: %w", db.path, renameErr)
	}
	return nil
}

func hashKey(h uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, h)
	return k
}

// u32len returns n — always a len() of something this package is about to
// write as a fixed-width uint32 field (a string, a slice of records) — as
// a uint32, panicking if n is negative or exceeds math.MaxUint32. Every
// caller either already checked a size bound before calling this (in which
// case the panic path is unreachable) or is encoding a value (a name, a
// path, a symbol count) that can never plausibly approach 4 GiB: either
// way, hitting the panic means an encode invariant this package's own
// format is supposed to guarantee was violated — a programmer error, not
// recoverable untrusted input.
func u32len(n int) uint32 {
	if n < 0 || n > math.MaxUint32 {
		panic(fmt.Sprintf("store: length %d out of uint32 range", n))
	}
	return uint32(n)
}

// FileStat is a lightweight (size, modification time) snapshot of one
// source file, taken the last time [UnitPointer] was recorded for it. It
// lets a later revalidation pass rule out an unchanged file by stat alone,
// without rereading its content.
type FileStat struct {
	Path         string
	Size         int64
	ModTimeNanos int64
}

// UnitPointer records what a package currently resolves to:
//
//   - BlobKey, the [CAS] key of its current [UnitBlob] (facts, export data,
//     and index entries) — a deterministic function of ContentHash plus
//     every direct workspace dependency's own current ExportHash (see
//     internal/index's key computation; this is the design's correctness
//     core, see plan-feat-v0.1.md).
//   - ContentHash, the package's own source content hash BlobKey was last
//     derived from.
//   - ExportHash, a hash of the package's current export data bytes —
//     folded into a *dependent's* BlobKey computation (not this package's
//     own), so a body-only edit (ContentHash changes, ExportHash does not)
//     never forces a dependent to recheck, while an API-changing edit
//     (ExportHash changes) always does.
//   - ToolchainFingerprint, the Go toolchain BlobKey was last computed
//     under, invalidating any of the above on an upgrade.
//   - Files, a per-file stat snapshot letting a revalidation pass skip
//     recomputing ContentHash by rereading file content when nothing on
//     disk looks touched.
type UnitPointer struct {
	BlobKey              uint64
	ContentHash          uint64
	ExportHash           uint64
	ToolchainFingerprint string
	Files                []FileStat
}

// encodeUnitPointer serializes p as:
//
//	[8]BlobKey [8]ContentHash [8]ExportHash [4]tcLen [tcLen]toolchain fingerprint [4]fileCount
//
// then per file [4]pathLen [pathLen]path [8]size [8]modTimeNanos.
func encodeUnitPointer(p UnitPointer) []byte {
	size := 32 + len(p.ToolchainFingerprint) // +4: fileCount, following the toolchain fingerprint bytes
	for _, f := range p.Files {
		size += 4 + len(f.Path) + 16
	}
	b := make([]byte, size)
	binary.LittleEndian.PutUint64(b[0:8], p.BlobKey)
	binary.LittleEndian.PutUint64(b[8:16], p.ContentHash)
	binary.LittleEndian.PutUint64(b[16:24], p.ExportHash)
	binary.LittleEndian.PutUint32(b[24:28], u32len(len(p.ToolchainFingerprint)))
	off := 28
	off += copy(b[off:], p.ToolchainFingerprint)
	binary.LittleEndian.PutUint32(b[off:], u32len(len(p.Files)))
	off += 4
	for _, f := range p.Files {
		binary.LittleEndian.PutUint32(b[off:], u32len(len(f.Path)))
		off += 4
		off += copy(b[off:], f.Path)
		// os.FileInfo never reports a negative size or mtime in practice;
		// clamp defensively rather than let a nonsensical negative value
		// wrap around when reinterpreted as unsigned.
		size := f.Size
		if size < 0 {
			size = 0
		}
		binary.LittleEndian.PutUint64(b[off:], uint64(size))
		off += 8
		mtime := f.ModTimeNanos
		if mtime < 0 {
			mtime = 0
		}
		binary.LittleEndian.PutUint64(b[off:], uint64(mtime))
		off += 8
	}
	return b
}

func decodeUnitPointer(b []byte) (UnitPointer, error) {
	if len(b) < 28 {
		return UnitPointer{}, fmt.Errorf("store: unit pointer record too short: %d bytes", len(b))
	}
	p := UnitPointer{
		BlobKey:     binary.LittleEndian.Uint64(b[0:8]),
		ContentHash: binary.LittleEndian.Uint64(b[8:16]),
		ExportHash:  binary.LittleEndian.Uint64(b[16:24]),
	}
	tcLen := int(binary.LittleEndian.Uint32(b[24:28]))
	off := 28
	if off+tcLen > len(b) {
		return UnitPointer{}, fmt.Errorf("store: unit pointer record truncated (toolchain fingerprint)")
	}
	p.ToolchainFingerprint = string(b[off : off+tcLen])
	off += tcLen
	if off+4 > len(b) {
		return UnitPointer{}, fmt.Errorf("store: unit pointer record truncated (file count)")
	}
	fileCount := binary.LittleEndian.Uint32(b[off:])
	off += 4
	files := make([]FileStat, 0, fileCount)
	for i := uint32(0); i < fileCount; i++ {
		if off+4 > len(b) {
			return UnitPointer{}, fmt.Errorf("store: unit pointer record truncated (file %d path length)", i)
		}
		pathLen := int(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		if off+pathLen+16 > len(b) {
			return UnitPointer{}, fmt.Errorf("store: unit pointer record truncated (file %d)", i)
		}
		path := string(b[off : off+pathLen])
		off += pathLen
		rawSize := binary.LittleEndian.Uint64(b[off:])
		off += 8
		rawMtime := binary.LittleEndian.Uint64(b[off:])
		off += 8
		if rawSize > math.MaxInt64 || rawMtime > math.MaxInt64 {
			return UnitPointer{}, fmt.Errorf("store: unit pointer record: file %d size/mtime out of range", i)
		}
		files = append(files, FileStat{Path: path, Size: int64(rawSize), ModTimeNanos: int64(rawMtime)})
	}
	p.Files = files
	return p, nil
}

// GetUnit returns the stored UnitPointer for pkgHash. It returns
// ErrNotFound if no entry exists. ctx is checked before the read starts, so
// a caller (e.g. internal/xref, mid a cancelable query) does not pay for a
// bbolt transaction its own context has already given up on.
func (db *DB) GetUnit(ctx context.Context, pkgHash uint64) (UnitPointer, error) {
	if err := ctx.Err(); err != nil {
		return UnitPointer{}, err
	}
	var p UnitPointer
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketUnit).Get(hashKey(pkgHash))
		if v == nil {
			return ErrNotFound
		}
		decoded, err := decodeUnitPointer(v)
		if err != nil {
			return err
		}
		p = decoded
		return nil
	})
	return p, err
}

// UnitEntry bundles one package's [UnitPointer] and the name/method/
// SymbolID-string index entries its current blob contributes, for an atomic
// write via [DB.PutUnit] or [DB.PutUnitsBatch].
type UnitEntry struct {
	PkgHash uint64
	Pointer UnitPointer
	Index   PackageIndexEntries
}

// PutUnit atomically stores e's pointer and index entries in a single
// transaction.
func (db *DB) PutUnit(e *UnitEntry) error {
	return db.PutUnitsBatch([]UnitEntry{*e})
}

// PutUnitsBatch atomically stores every entry's pointer and index entries in
// a single transaction. Use this for index-build time bulk writes instead of
// calling PutUnit in a loop, to avoid one commit per package.
func (db *DB) PutUnitsBatch(entries []UnitEntry) error {
	return db.bolt.Update(func(tx *bbolt.Tx) error {
		unitB := tx.Bucket(bucketUnit)
		for i := range entries {
			e := &entries[i]
			if err := unitB.Put(hashKey(e.PkgHash), encodeUnitPointer(e.Pointer)); err != nil {
				return err
			}
			if err := applyIndexEntries(tx, e.PkgHash, &e.Index); err != nil {
				return err
			}
		}
		return nil
	})
}

// PutUnitPointersBatch stores every (pkgHash, UnitPointer) pair in entries
// in a single transaction, touching only the unit bucket — unlike
// [DB.PutUnitsBatch], this never writes name/method/SymbolID-string index
// entries. Use this to refresh a package's stat snapshot alone (its BlobKey
// and ContentHash unchanged, confirmed via the content-hash fallback after a
// stat mismatch — see internal/index), since its blob's index entries are
// already correctly recorded from when that BlobKey was first written.
func (db *DB) PutUnitPointersBatch(entries map[uint64]UnitPointer) error {
	return db.bolt.Update(func(tx *bbolt.Tx) error {
		unitB := tx.Bucket(bucketUnit)
		for hash, p := range entries {
			if err := unitB.Put(hashKey(hash), encodeUnitPointer(p)); err != nil {
				return err
			}
		}
		return nil
	})
}

// PutBuildFingerprint records fp — typically runtime.Version(), the
// toolchain a full build run just checked every package against — as this
// database's whole-index build fingerprint, overwriting any previous value.
// A later caller (internal/server's warm-start check) compares this against
// the running toolchain to decide whether the database is trustworthy
// enough to open directly, skipping a full rebuild pass.
func (db *DB) PutBuildFingerprint(fp string) error {
	return db.bolt.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(buildFingerprintKey, []byte(fp))
	})
}

// BuildFingerprint returns the fingerprint last recorded via
// PutBuildFingerprint. It returns ErrNotFound if none has been recorded
// yet (e.g. a brand new database, or a prior run that never completed
// successfully).
func (db *DB) BuildFingerprint() (string, error) {
	var fp string
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketMeta).Get(buildFingerprintKey)
		if v == nil {
			return ErrNotFound
		}
		fp = string(v)
		return nil
	})
	return fp, err
}

// AddNameSymbol records that idHash is a symbol named name, for
// workspace/symbol and unimported-completion prefix lookups via
// [DB.LookupNamePrefix]. name is matched case-insensitively (stored
// lowercased).
func (db *DB) AddNameSymbol(name string, idHash uint64) error {
	key := []byte(strings.ToLower(name))
	return db.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		cur := b.Get(key)
		if containsUint64(cur, idHash) {
			return nil
		}
		return b.Put(key, appendUint64(cur, idHash))
	})
}

// LookupNamePrefix returns, for every stored name with the given prefix
// (matched case-insensitively), the list of symbolIDHash values recorded
// under it via [DB.AddNameSymbol]. ctx is checked before the scan starts
// (see [DB.GetUnit]'s doc).
func (db *DB) LookupNamePrefix(ctx context.Context, prefix string) (map[string][]uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lower := []byte(strings.ToLower(prefix))
	result := make(map[string][]uint64)
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketName).Cursor()
		for k, v := c.Seek(lower); k != nil && strings.HasPrefix(string(k), string(lower)); k, v = c.Next() {
			result[string(k)] = decodeUint64List(v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// MethodEntry identifies one method: its receiver type's defining package
// and own SymbolID hash (PkgHash, TypeSymbolIDHash), plus — since the
// unexported-implementer fix — enough about the method itself to confirm
// interface satisfaction and resolve its own declaration without ever
// decoding the receiver type's export data:
//
//   - MethodPkgHash/MethodIDHash identify the method's OWN definition (its
//     [Symbol] entry in MethodPkgHash's facts blob), resolvable directly —
//     no export data involved. These differ from PkgHash/TypeSymbolIDHash
//     for a promoted method (one the receiver type gets via struct
//     embedding): MethodPkgHash is then the EMBEDDED type's defining
//     package, since that is where the method is actually declared (see
//     internal/xref's methodFuncSymbol, whose decode-time computation this
//     mirrors at index time instead).
//   - Fingerprint is a hash of the method's canonical, fully-package-
//     qualified signature (see internal/index's registerMethodSet/
//     MethodFingerprint) — comparable across independently-indexed packages
//     without decoding either side's export data. It is 0 for a method
//     whose receiver has type parameters (a generic type is not indexed
//     with a fingerprint at all; see registerMethodSet's doc), the sentinel
//     internal/xref's confirmation step treats as "must fall back to
//     decoding this candidate instead."
type MethodEntry struct {
	PkgHash          uint64
	TypeSymbolIDHash uint64
	MethodPkgHash    uint64
	MethodIDHash     uint64
	Fingerprint      uint64
}

// AddMethodSymbol records that a method named methodName exists with
// receiver type e.TypeSymbolIDHash in package e.PkgHash. Callers use this
// as the sound first-pass filter for implementation queries: the
// interface -> implementers direction (internal/xref's implementingTypes)
// confirms a survivor by comparing e.Fingerprint against the interface's
// own, needing no further decode; a candidate whose receiver is generic
// (e.Fingerprint == 0) or whose fingerprint does not match still falls back
// to loading export data and confirming with types.Implements, exactly as
// every candidate used to.
func (db *DB) AddMethodSymbol(methodName string, e MethodEntry) error {
	key := []byte(methodName)
	return db.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMethod)
		cur := b.Get(key)
		if containsMethodEntry(cur, e) {
			return nil
		}
		return b.Put(key, appendMethodEntry(cur, e))
	})
}

// LookupMethod returns every MethodEntry recorded for methodName. ctx is
// checked before the read starts (see [DB.GetUnit]'s doc).
func (db *DB) LookupMethod(ctx context.Context, methodName string) ([]MethodEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var entries []MethodEntry
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketMethod).Get([]byte(methodName))
		entries = decodeMethodEntryList(v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// PutSymbolIDString records symbolID as a known source string for idHash =
// Hash(symbolID). Because Hash is a 64-bit hash, two different strings can
// map to the same idHash; storing every known string lets
// [DB.VerifySymbolIDString] tell a genuine match from a collision.
func (db *DB) PutSymbolIDString(idHash uint64, symbolID string) error {
	key := hashKey(idHash)
	return db.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSymStr)
		cur := b.Get(key)
		for _, s := range decodeStringList(cur) {
			if s == symbolID {
				return nil
			}
		}
		return b.Put(key, appendStringList(cur, symbolID))
	})
}

// SymbolIDStrings returns every SymbolID string previously recorded for
// idHash via [DB.PutSymbolIDString]. More than one entry means idHash is a
// collision between genuinely different symbols; callers that need a single
// symbol should pick the entry that resolves (e.g. its package's facts blob
// actually contains a symbol with this idHash). ctx is checked before the
// read starts (see [DB.GetUnit]'s doc).
func (db *DB) SymbolIDStrings(ctx context.Context, idHash uint64) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []string
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		out = decodeStringList(tx.Bucket(bucketSymStr).Get(hashKey(idHash)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// VerifySymbolIDString reports whether symbolID was previously stored for
// idHash via [DB.PutSymbolIDString]. A false result for an idHash that has
// other strings stored under it indicates a hash collision, not that
// symbolID is unknown to the index.
func (db *DB) VerifySymbolIDString(idHash uint64, symbolID string) (bool, error) {
	var found bool
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketSymStr).Get(hashKey(idHash))
		for _, s := range decodeStringList(v) {
			if s == symbolID {
				found = true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// NameEntry is one [DB.AddNameSymbol] write, batched via a [UnitEntry]'s
// Index field.
type NameEntry struct {
	Name   string
	IDHash uint64
}

// MethodSymbolEntry is one [DB.AddMethodSymbol] write, batched via a
// [UnitEntry]'s Index field.
type MethodSymbolEntry struct {
	Name  string
	Entry MethodEntry
}

// SymStrEntry is one [DB.PutSymbolIDString] write, batched via a
// [UnitEntry]'s Index field.
type SymStrEntry struct {
	IDHash   uint64
	SymbolID string
}

// PackageIndexEntries collects the name-index, method-index, and symbol-ID
// string entries produced while extracting one package's facts. It is
// stored inside that package's [UnitBlob] (see the CAS key's doc) so that
// re-pointing a package at an already-known blob — the CAS-hit fast path,
// e.g. switching back to a previously-seen branch — can repopulate this
// database's indices without re-type-checking anything.
type PackageIndexEntries struct {
	Names    []NameEntry
	Methods  []MethodSymbolEntry
	SymStrs  []SymStrEntry
	Postings []PostingEntry
}

// applyIndexEntries writes entries' name-index, method-index, and
// SymbolID-string entries into tx's buckets, deduplicating each posting
// list against its existing content the same way AddNameSymbol,
// AddMethodSymbol, and PutSymbolIDString do individually. Entries are never
// removed when a package's blob changes: a stale entry pointing at a since-
// overwritten symbol is harmless (see internal/xref's symbolByHash, which
// silently skips a lookup that no longer resolves) and self-heals the next
// time that name is genuinely looked up, so there is nothing to reconcile
// here beyond appending this blob's own contribution.
//
// entries.Postings is different: References must never see a stale
// location a source file no longer actually contains, so applyPostings
// replaces srcPkgHash's ENTIRE prior contribution to the postings index
// exactly, rather than appending alongside it — see its own doc.
func applyIndexEntries(tx *bbolt.Tx, srcPkgHash uint64, entries *PackageIndexEntries) error {
	nameB := tx.Bucket(bucketName)
	for _, n := range entries.Names {
		key := []byte(strings.ToLower(n.Name))
		cur := nameB.Get(key)
		if containsUint64(cur, n.IDHash) {
			continue
		}
		if err := nameB.Put(key, appendUint64(cur, n.IDHash)); err != nil {
			return err
		}
	}

	methodB := tx.Bucket(bucketMethod)
	for _, m := range entries.Methods {
		key := []byte(m.Name)
		cur := methodB.Get(key)
		if containsMethodEntry(cur, m.Entry) {
			continue
		}
		if err := methodB.Put(key, appendMethodEntry(cur, m.Entry)); err != nil {
			return err
		}
	}

	symB := tx.Bucket(bucketSymStr)
	for _, s := range entries.SymStrs {
		key := hashKey(s.IDHash)
		cur := symB.Get(key)
		dup := false
		for _, existing := range decodeStringList(cur) {
			if existing == s.SymbolID {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		if err := symB.Put(key, appendStringList(cur, s.SymbolID)); err != nil {
			return err
		}
	}

	return applyPostings(tx, srcPkgHash, entries.Postings)
}

// --- fixed-width list encodings for posting-list bucket values ---

func appendUint64(list []byte, v uint64) []byte {
	out := make([]byte, len(list)+8)
	copy(out, list)
	binary.LittleEndian.PutUint64(out[len(list):], v)
	return out
}

func decodeUint64List(list []byte) []uint64 {
	if len(list) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(list)/8)
	for i := 0; i+8 <= len(list); i += 8 {
		out = append(out, binary.LittleEndian.Uint64(list[i:i+8]))
	}
	return out
}

func containsUint64(list []byte, v uint64) bool {
	for _, x := range decodeUint64List(list) {
		if x == v {
			return true
		}
	}
	return false
}

const methodEntrySize = 40

func appendMethodEntry(list []byte, e MethodEntry) []byte {
	out := make([]byte, len(list)+methodEntrySize)
	copy(out, list)
	binary.LittleEndian.PutUint64(out[len(list):], e.PkgHash)
	binary.LittleEndian.PutUint64(out[len(list)+8:], e.TypeSymbolIDHash)
	binary.LittleEndian.PutUint64(out[len(list)+16:], e.MethodPkgHash)
	binary.LittleEndian.PutUint64(out[len(list)+24:], e.MethodIDHash)
	binary.LittleEndian.PutUint64(out[len(list)+32:], e.Fingerprint)
	return out
}

func decodeMethodEntryList(list []byte) []MethodEntry {
	if len(list) == 0 {
		return nil
	}
	out := make([]MethodEntry, 0, len(list)/methodEntrySize)
	for i := 0; i+methodEntrySize <= len(list); i += methodEntrySize {
		out = append(out, MethodEntry{
			PkgHash:          binary.LittleEndian.Uint64(list[i:]),
			TypeSymbolIDHash: binary.LittleEndian.Uint64(list[i+8:]),
			MethodPkgHash:    binary.LittleEndian.Uint64(list[i+16:]),
			MethodIDHash:     binary.LittleEndian.Uint64(list[i+24:]),
			Fingerprint:      binary.LittleEndian.Uint64(list[i+32:]),
		})
	}
	return out
}

func containsMethodEntry(list []byte, e MethodEntry) bool {
	for _, x := range decodeMethodEntryList(list) {
		if x == e {
			return true
		}
	}
	return false
}

// string list: repeated [uint32 len][bytes].

func appendStringList(list []byte, s string) []byte {
	out := make([]byte, len(list)+4+len(s))
	copy(out, list)
	binary.LittleEndian.PutUint32(out[len(list):], u32len(len(s)))
	copy(out[len(list)+4:], s)
	return out
}

func decodeStringList(list []byte) []string {
	var out []string
	for i := 0; i+4 <= len(list); {
		n := int(binary.LittleEndian.Uint32(list[i:]))
		i += 4
		if i+n > len(list) {
			break
		}
		out = append(out, string(list[i:i+n]))
		i += n
	}
	return out
}
