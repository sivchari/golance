package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"go.etcd.io/bbolt"
)

// ErrNotFound is returned by Get-style lookups that find no entry for the
// given key.
var ErrNotFound = errors.New("store: not found")

var (
	bucketUnit   = []byte("unit")   // pkgPathHash -> UnitPointer (see PutUnit)
	bucketMeta   = []byte("meta")   // reserved keys, e.g. buildFingerprintKey
	bucketName   = []byte("name")   // lowercased symbol name -> []symbolIDHash
	bucketMethod = []byte("method") // method name -> []MethodEntry
	bucketSymStr = []byte("symstr") // symbolIDHash -> []original SymbolID string
)

var allBuckets = [][]byte{bucketUnit, bucketMeta, bucketName, bucketMethod, bucketSymStr}

// buildFingerprintKey is a reserved bucketMeta key for the whole
// database's build fingerprint (see PutBuildFingerprint).
var buildFingerprintKey = []byte("\x00golance:fingerprint")

// DB is a handle to golance's per-root index: a small bbolt database mapping
// each workspace package to its current [CAS] blob key (see [UnitPointer]),
// plus the name/method/SymbolID-string lookup indices built from those
// blobs' contents. It holds no facts or export data itself — those live in
// the shared, lock-free [CAS] this root's session opens alongside it (see
// the package doc). A *DB is safe for concurrent use by multiple
// goroutines, and — being per-root/per-worktree rather than shared across a
// whole repository — never contended by another golance session the way the
// old single shared bbolt database was.
type DB struct {
	bolt *bbolt.DB
}

// Open opens (creating if necessary) the index database at path and ensures
// all buckets exist. Unlike the pre-CAS design, this is always a per-root
// file: nothing about it is ever shared across worktrees, so there is
// nothing to lock or wait for.
func Open(path string) (*DB, error) {
	bdb, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	err = bdb.Update(func(tx *bbolt.Tx) error {
		for _, b := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = bdb.Close()
		return nil, fmt.Errorf("store: init buckets: %w", err)
	}
	return &DB{bolt: bdb}, nil
}

// Close closes the underlying database file.
func (db *DB) Close() error { return db.bolt.Close() }

func hashKey(h uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, h)
	return k
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
	binary.LittleEndian.PutUint32(b[24:28], uint32(len(p.ToolchainFingerprint)))
	off := 28
	off += copy(b[off:], p.ToolchainFingerprint)
	binary.LittleEndian.PutUint32(b[off:], uint32(len(p.Files)))
	off += 4
	for _, f := range p.Files {
		binary.LittleEndian.PutUint32(b[off:], uint32(len(f.Path)))
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
// ErrNotFound if no entry exists.
func (db *DB) GetUnit(pkgHash uint64) (UnitPointer, error) {
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
			if err := applyIndexEntries(tx, e.Index); err != nil {
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
// under it via [DB.AddNameSymbol].
func (db *DB) LookupNamePrefix(prefix string) (map[string][]uint64, error) {
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

// MethodEntry identifies one method's receiver type and defining package,
// as recorded via [DB.AddMethodSymbol].
type MethodEntry struct {
	PkgHash          uint64
	TypeSymbolIDHash uint64
}

// AddMethodSymbol records that a method named methodName exists with
// receiver type e.TypeSymbolIDHash in package e.PkgHash. Callers use this
// as the sound first-pass filter for implementation queries, then load
// export data for the surviving candidates to confirm with types.Implements.
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

// LookupMethod returns every MethodEntry recorded for methodName.
func (db *DB) LookupMethod(methodName string) ([]MethodEntry, error) {
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
// actually contains a symbol with this idHash).
func (db *DB) SymbolIDStrings(idHash uint64) ([]string, error) {
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
	Names   []NameEntry
	Methods []MethodSymbolEntry
	SymStrs []SymStrEntry
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
func applyIndexEntries(tx *bbolt.Tx, entries PackageIndexEntries) error {
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
	return nil
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

const methodEntrySize = 16

func appendMethodEntry(list []byte, e MethodEntry) []byte {
	out := make([]byte, len(list)+methodEntrySize)
	copy(out, list)
	binary.LittleEndian.PutUint64(out[len(list):], e.PkgHash)
	binary.LittleEndian.PutUint64(out[len(list)+8:], e.TypeSymbolIDHash)
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
	binary.LittleEndian.PutUint32(out[len(list):], uint32(len(s)))
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
