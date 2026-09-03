package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	"go.etcd.io/bbolt"
)

// PostingEntry is one location, recorded while extracting srcPkgHash's facts
// (see internal/index's extractFacts/addRef), that references the symbol
// identified by (TargetPkgHash, TargetIDHash). A package's facts extraction
// pass produces one PostingEntry per outgoing reference — the same
// information [RefInput] already carries, plus the reference's own resolved
// file path (RefInput only carries a FileIdx into the source package's own
// facts blob, which a posting — stored independently of any one facts blob —
// cannot dereference). Entries are batched via a [PackageIndexEntries]'
// Postings field, alongside Names/Methods/SymStrs, exactly as one package's
// contribution to [DB]'s reverse reference index.
type PostingEntry struct {
	TargetPkgHash uint64
	TargetIDHash  uint64
	File          string
	Line          uint32
	Col           uint32
	EndCol        uint32
}

// PostingLocation is one reference location decoded back out of the
// postings index: a PostingEntry with its (TargetPkgHash, TargetIDHash,
// srcPkgHash) key stripped, since a caller scanning [DB.PostingsFor] already
// knows the target it queried for and receives the source package hash
// separately via [PostingRecord].
type PostingLocation struct {
	File   string
	Line   uint32
	Col    uint32
	EndCol uint32
}

// PostingRecord is one (targetPkgHash, targetIDHash, srcPkgHash) posting
// entry decoded by [DB.PostingsFor]: every location within SrcPkgHash that
// references the queried target, plus Bytes — the encoded size of this
// record's stored value — for a caller's own read-size accounting (see
// internal/xref's StatsSink).
type PostingRecord struct {
	SrcPkgHash uint64
	Bytes      int64
	Locations  []PostingLocation
}

// postingGroupKey identifies one (targetPkgHash, targetIDHash) pair: the
// unit locationsForAll queries by, and the unit a source package's postings
// are grouped into before being written (see applyPostings).
type postingGroupKey struct {
	TargetPkgHash uint64
	TargetIDHash  uint64
}

// postingKey encodes the full, unique bbolt key for one posting record:
// target's (PkgHash, IDHash) — big-endian, matching hashKey, so
// postingPrefix's byte range covers exactly this target regardless of
// srcPkgHash — followed by srcPkgHash itself. bbolt orders keys
// lexicographically, so every record for one target sorts contiguously
// under postingPrefix(targetPkgHash, targetIDHash), letting
// [DB.PostingsFor] answer with a single prefix scan instead of a full
// bucket walk.
func postingKey(targetPkgHash, targetIDHash, srcPkgHash uint64) []byte {
	k := make([]byte, 24)
	binary.BigEndian.PutUint64(k[0:8], targetPkgHash)
	binary.BigEndian.PutUint64(k[8:16], targetIDHash)
	binary.BigEndian.PutUint64(k[16:24], srcPkgHash)
	return k
}

// postingPrefix returns the common key prefix shared by every posting
// record for (targetPkgHash, targetIDHash), for [DB.PostingsFor]'s prefix
// scan and for stripping a scanned key down to its trailing srcPkgHash.
func postingPrefix(targetPkgHash, targetIDHash uint64) []byte {
	k := make([]byte, 16)
	binary.BigEndian.PutUint64(k[0:8], targetPkgHash)
	binary.BigEndian.PutUint64(k[8:16], targetIDHash)
	return k
}

// srcHashFromPostingKey extracts the trailing srcPkgHash from a full
// postingKey, the counterpart of postingPrefix's leading 16 bytes.
func srcHashFromPostingKey(key []byte) uint64 {
	return binary.BigEndian.Uint64(key[16:24])
}

// encodePostingLocations serializes locs as:
//
//	[4]locCount
//	locCount * { [4]pathLen [pathLen]path [4]line [4]col [4]endCol }
func encodePostingLocations(locs []PostingLocation) []byte {
	size := 4
	for _, l := range locs {
		size += 4 + len(l.File) + 12
	}
	b := make([]byte, size)
	binary.LittleEndian.PutUint32(b[0:4], u32len(len(locs)))
	off := 4
	for _, l := range locs {
		binary.LittleEndian.PutUint32(b[off:], u32len(len(l.File)))
		off += 4
		off += copy(b[off:], l.File)
		binary.LittleEndian.PutUint32(b[off:], l.Line)
		off += 4
		binary.LittleEndian.PutUint32(b[off:], l.Col)
		off += 4
		binary.LittleEndian.PutUint32(b[off:], l.EndCol)
		off += 4
	}
	return b
}

// decodePostingLocations parses b as encodePostingLocations produced it.
func decodePostingLocations(b []byte) ([]PostingLocation, error) {
	count, off, err := takeUint32(b, 0)
	if err != nil {
		return nil, fmt.Errorf("store: posting record count: %w", err)
	}
	locs := make([]PostingLocation, count)
	for i := range locs {
		var file string
		file, off, err = takeString(b, off)
		if err != nil {
			return nil, fmt.Errorf("store: posting location %d: %w", i, err)
		}
		var line, col, endCol int
		line, off, err = takeUint32(b, off)
		if err != nil {
			return nil, fmt.Errorf("store: posting location %d line: %w", i, err)
		}
		col, off, err = takeUint32(b, off)
		if err != nil {
			return nil, fmt.Errorf("store: posting location %d col: %w", i, err)
		}
		endCol, off, err = takeUint32(b, off)
		if err != nil {
			return nil, fmt.Errorf("store: posting location %d endCol: %w", i, err)
		}
		lineU32, err := uint32Field(line, "posting location line")
		if err != nil {
			return nil, err
		}
		colU32, err := uint32Field(col, "posting location col")
		if err != nil {
			return nil, err
		}
		endColU32, err := uint32Field(endCol, "posting location endCol")
		if err != nil {
			return nil, err
		}
		locs[i] = PostingLocation{File: file, Line: lineU32, Col: colU32, EndCol: endColU32}
	}
	return locs, nil
}

// appendManifestEntry appends one (targetPkgHash, targetIDHash) pair to
// list, the fixed-16-byte-per-entry encoding of a srcPkgHash's posting
// manifest (see applyPostings).
func appendManifestEntry(list []byte, k postingGroupKey) []byte {
	out := make([]byte, len(list)+16)
	copy(out, list)
	binary.LittleEndian.PutUint64(out[len(list):], k.TargetPkgHash)
	binary.LittleEndian.PutUint64(out[len(list)+8:], k.TargetIDHash)
	return out
}

// decodeManifest decodes a srcPkgHash's posting manifest, the list of
// (targetPkgHash, targetIDHash) keys its own last-written postings are
// stored under.
func decodeManifest(list []byte) []postingGroupKey {
	if len(list) == 0 {
		return nil
	}
	out := make([]postingGroupKey, 0, len(list)/16)
	for i := 0; i+16 <= len(list); i += 16 {
		out = append(out, postingGroupKey{
			TargetPkgHash: binary.LittleEndian.Uint64(list[i:]),
			TargetIDHash:  binary.LittleEndian.Uint64(list[i+8:]),
		})
	}
	return out
}

// applyPostings replaces srcPkgHash's entire contribution to the postings
// index with postings, inside tx: every posting record this same
// srcPkgHash wrote on a previous call (found via its manifest, decoded and
// deleted key-by-key — bbolt cannot prefix-scan by a key's SUFFIX, which is
// where srcPkgHash sits, so the manifest is what makes an exact, targeted
// delete possible without a full bucket walk) is removed, postings' entries
// are grouped by target and written fresh, and a new manifest recording
// exactly those targets is stored — so a package's postings never lag or
// outlive the facts they mirror (see [DB.PutUnitsBatch]'s doc: this runs in
// the same transaction as that package's own unit-pointer write).
func applyPostings(tx *bbolt.Tx, srcPkgHash uint64, postings []PostingEntry) error {
	postB := tx.Bucket(bucketRefPostings)
	manifestB := tx.Bucket(bucketRefPostingManifest)

	srcKey := hashKey(srcPkgHash)
	for _, stale := range decodeManifest(manifestB.Get(srcKey)) {
		if err := postB.Delete(postingKey(stale.TargetPkgHash, stale.TargetIDHash, srcPkgHash)); err != nil {
			return err
		}
	}

	groups := make(map[postingGroupKey][]PostingLocation, len(postings))
	for _, p := range postings {
		k := postingGroupKey{TargetPkgHash: p.TargetPkgHash, TargetIDHash: p.TargetIDHash}
		groups[k] = append(groups[k], PostingLocation{File: p.File, Line: p.Line, Col: p.Col, EndCol: p.EndCol})
	}

	targets := make([]postingGroupKey, 0, len(groups))
	for k := range groups {
		targets = append(targets, k)
	}
	// Deterministic write order only; bbolt itself does not require it.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].TargetPkgHash != targets[j].TargetPkgHash {
			return targets[i].TargetPkgHash < targets[j].TargetPkgHash
		}
		return targets[i].TargetIDHash < targets[j].TargetIDHash
	})

	var manifest []byte
	for _, k := range targets {
		if err := postB.Put(postingKey(k.TargetPkgHash, k.TargetIDHash, srcPkgHash), encodePostingLocations(groups[k])); err != nil {
			return err
		}
		manifest = appendManifestEntry(manifest, k)
	}
	return manifestB.Put(srcKey, manifest)
}

// PostingsFor returns every [PostingRecord] referencing the symbol
// identified by (targetPkgHash, targetIDHash) — one per distinct source
// package that references it — via a single bbolt prefix scan over
// postingPrefix(targetPkgHash, targetIDHash): O(result size), unlike a
// reverse-dependency closure walk over every package that COULD reference
// the symbol. ctx is checked before the scan starts (see [DB.GetUnit]'s
// doc).
func (db *DB) PostingsFor(ctx context.Context, targetPkgHash, targetIDHash uint64) ([]PostingRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix := postingPrefix(targetPkgHash, targetIDHash)
	var out []PostingRecord
	err := db.bolt.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketRefPostings).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			locs, err := decodePostingLocations(v)
			if err != nil {
				return fmt.Errorf("store: decode postings for target %d/%d: %w", targetPkgHash, targetIDHash, err)
			}
			out = append(out, PostingRecord{SrcPkgHash: srcHashFromPostingKey(k), Bytes: int64(len(v)), Locations: locs})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
