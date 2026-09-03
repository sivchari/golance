package store

import (
	"encoding/binary"
	"fmt"
	"math"
)

// UnitBlob is the immutable, content-addressed record for one package's
// exact source version: its facts blob (see [Builder]/[View]) and export
// data verbatim, plus everything a later revalidation pass needs without
// re-type-checking anything — a per-file stat snapshot and the name/method/
// SymbolID-string index entries this version contributes. It is stored
// whole under one [CAS] key; see the package doc for what that key covers.
type UnitBlob struct {
	Facts  []byte
	Export []byte
	Files  []FileStat
	Index  PackageIndexEntries
}

const (
	unitMagic = "GLUB"
	// unitVersion is bumped to 3 for the reverse reference index's
	// PostingEntry section (see EncodeUnitBlob's doc and schemaVersion's
	// matching bump): a version-2 blob has no postings section at all, and
	// internal/index's factsSchemaVersion forces every CAS key to change so
	// a fresh build never asks casHitOutcome to decode a stale version-2
	// blob (which would otherwise decode "successfully" with zero
	// postings, silently under-indexing every already-built package instead
	// of erroring outright).
	//
	// Bumped to 2 alongside [MethodEntry]'s three new fields (see its doc
	// and schemaVersion's matching bump): a version-1 blob's methods
	// section is encoded one 16-byte record per method, this package's own
	// decodeMethodEntries now expects (and internal/index's
	// factsSchemaVersion forces every CAS key to change so a fresh build
	// never asks casHitOutcome to decode a stale version-1 blob at all).
	unitVersion = 3
	// unitHeaderSize is EncodeUnitBlob's fixed-size header length (magic,
	// version, reserved, factsLen, exportLen, fileCount, namesCount -- see
	// its doc for the exact byte layout). Facts begins immediately after
	// it, which is what makes [UnitFactsRange] possible: a caller that
	// wants only Facts can read this many header bytes plus exactly
	// factsLen more, instead of the whole blob.
	unitHeaderSize = 24
)

// UnitFactsRange validates header -- a unit blob's first unitHeaderSize
// bytes are enough, the whole blob is not required -- and returns the byte
// offset and length of its Facts section within the blob:
// [offset, offset+length). DecodeUnitBlob uses it for its own header
// validation; a caller that only wants to know how large a blob's Facts
// section is (without decoding the whole blob) can use it directly, the
// same way internal/xref's closure-scale benchmarks report it for scale
// context.
func UnitFactsRange(header []byte) (offset, length int, err error) {
	if len(header) < unitHeaderSize || string(header[0:4]) != unitMagic {
		return 0, 0, fmt.Errorf("store: bad unit blob header")
	}
	version := binary.LittleEndian.Uint16(header[4:6])
	if version != unitVersion {
		return 0, 0, fmt.Errorf("store: unsupported unit blob version %d (want %d)", version, unitVersion)
	}
	factsLen := int(binary.LittleEndian.Uint32(header[8:12]))
	return unitHeaderSize, factsLen, nil
}

// EncodeUnitBlob serializes u as:
//
//	[4]magic [2]version [2]reserved
//	[4]factsLen [4]exportLen [4]fileCount [4]namesCount [4]methodsCount [4]symstrCount
//	[factsLen]facts [exportLen]export
//	fileCount * { [4]pathLen [pathLen]path [8]size [8]modTimeNanos }
//	namesCount * { [4]nameLen [nameLen]name [8]idHash }
//	methodsCount * { [4]nameLen [nameLen]name [8]pkgHash [8]typeSymbolIDHash [8]methodPkgHash [8]methodIDHash [8]fingerprint }
//	symstrCount * { [8]idHash [4]symbolIDLen [symbolIDLen]symbolID }
//	postingsCount * { [8]targetPkgHash [8]targetIDHash [4]pathLen [pathLen]path [4]line [4]col [4]endCol }
func EncodeUnitBlob(u *UnitBlob) []byte {
	size := 24 + 12 + len(u.Facts) + len(u.Export) // +12: methodsCount, symstrCount, postingsCount (see putIndexEntries)
	for _, f := range u.Files {
		size += 4 + len(f.Path) + 16
	}
	for _, n := range u.Index.Names {
		size += 4 + len(n.Name) + 8
	}
	for _, m := range u.Index.Methods {
		size += 4 + len(m.Name) + 40
	}
	for _, s := range u.Index.SymStrs {
		size += 8 + 4 + len(s.SymbolID)
	}
	for _, p := range u.Index.Postings {
		size += 16 + 4 + len(p.File) + 12
	}

	b := make([]byte, size)
	copy(b[0:4], unitMagic)
	binary.LittleEndian.PutUint16(b[4:6], unitVersion)
	binary.LittleEndian.PutUint32(b[8:12], u32len(len(u.Facts)))
	binary.LittleEndian.PutUint32(b[12:16], u32len(len(u.Export)))
	binary.LittleEndian.PutUint32(b[16:20], u32len(len(u.Files)))
	binary.LittleEndian.PutUint32(b[20:24], u32len(len(u.Index.Names)))
	off := 24
	off += copy(b[off:], u.Facts)
	off += copy(b[off:], u.Export)
	for _, f := range u.Files {
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
	off = putIndexEntries(b, off, &u.Index)
	return b[:off]
}

// putIndexEntries appends the methodsCount/symstrCount-prefixed sections
// following the fixed-size header's namesCount-and-earlier fields: the
// header only reserves space for namesCount because methods/symstrs counts
// are folded into the trailing sections themselves (see decodeUnitBlob).
func putIndexEntries(b []byte, off int, idx *PackageIndexEntries) int {
	for _, n := range idx.Names {
		binary.LittleEndian.PutUint32(b[off:], u32len(len(n.Name)))
		off += 4
		off += copy(b[off:], n.Name)
		binary.LittleEndian.PutUint64(b[off:], n.IDHash)
		off += 8
	}
	binary.LittleEndian.PutUint32(b[off:], u32len(len(idx.Methods)))
	off += 4
	for _, m := range idx.Methods {
		binary.LittleEndian.PutUint32(b[off:], u32len(len(m.Name)))
		off += 4
		off += copy(b[off:], m.Name)
		binary.LittleEndian.PutUint64(b[off:], m.Entry.PkgHash)
		off += 8
		binary.LittleEndian.PutUint64(b[off:], m.Entry.TypeSymbolIDHash)
		off += 8
		binary.LittleEndian.PutUint64(b[off:], m.Entry.MethodPkgHash)
		off += 8
		binary.LittleEndian.PutUint64(b[off:], m.Entry.MethodIDHash)
		off += 8
		binary.LittleEndian.PutUint64(b[off:], m.Entry.Fingerprint)
		off += 8
	}
	binary.LittleEndian.PutUint32(b[off:], u32len(len(idx.SymStrs)))
	off += 4
	for _, s := range idx.SymStrs {
		binary.LittleEndian.PutUint64(b[off:], s.IDHash)
		off += 8
		binary.LittleEndian.PutUint32(b[off:], u32len(len(s.SymbolID)))
		off += 4
		off += copy(b[off:], s.SymbolID)
	}
	binary.LittleEndian.PutUint32(b[off:], u32len(len(idx.Postings)))
	off += 4
	for _, p := range idx.Postings {
		binary.LittleEndian.PutUint64(b[off:], p.TargetPkgHash)
		off += 8
		binary.LittleEndian.PutUint64(b[off:], p.TargetIDHash)
		off += 8
		binary.LittleEndian.PutUint32(b[off:], u32len(len(p.File)))
		off += 4
		off += copy(b[off:], p.File)
		binary.LittleEndian.PutUint32(b[off:], p.Line)
		off += 4
		binary.LittleEndian.PutUint32(b[off:], p.Col)
		off += 4
		binary.LittleEndian.PutUint32(b[off:], p.EndCol)
		off += 4
	}
	return off
}

// DecodeUnitBlob parses b as EncodeUnitBlob produced it. Facts and Export
// alias b (no copy); callers that need them to outlive b's own lifetime
// (e.g. a []byte returned from [CAS.Get], safe to retain since it is a fresh
// os.ReadFile result, not a memory-mapped transaction view) may keep them as
// is.
func DecodeUnitBlob(b []byte) (UnitBlob, error) {
	off, factsLen, err := UnitFactsRange(b)
	if err != nil {
		return UnitBlob{}, err
	}
	exportLen := int(binary.LittleEndian.Uint32(b[12:16]))
	fileCount := int(binary.LittleEndian.Uint32(b[16:20]))
	nameCount := int(binary.LittleEndian.Uint32(b[20:24]))

	facts, off, err := takeBytes(b, off, factsLen)
	if err != nil {
		return UnitBlob{}, fmt.Errorf("store: unit blob facts: %w", err)
	}
	export, off, err := takeBytes(b, off, exportLen)
	if err != nil {
		return UnitBlob{}, fmt.Errorf("store: unit blob export: %w", err)
	}

	files, off, err := decodeFileStats(b, off, fileCount)
	if err != nil {
		return UnitBlob{}, err
	}
	names, off, err := decodeNameEntries(b, off, nameCount)
	if err != nil {
		return UnitBlob{}, err
	}
	methods, off, err := decodeMethodEntries(b, off)
	if err != nil {
		return UnitBlob{}, err
	}
	symStrs, off, err := decodeSymStrEntries(b, off)
	if err != nil {
		return UnitBlob{}, err
	}
	postings, _, err := decodePostingEntries(b, off)
	if err != nil {
		return UnitBlob{}, err
	}

	return UnitBlob{
		Facts:  facts,
		Export: export,
		Files:  files,
		Index:  PackageIndexEntries{Names: names, Methods: methods, SymStrs: symStrs, Postings: postings},
	}, nil
}

// decodeFileStats decodes count fixed-format FileStat records starting at
// off (see EncodeUnitBlob's doc).
func decodeFileStats(b []byte, off, count int) ([]FileStat, int, error) {
	files := make([]FileStat, count)
	var err error
	for i := range files {
		var path string
		path, off, err = takeString(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob file %d: %w", i, err)
		}
		var rawSize, rawMtime uint64
		rawSize, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob file %d size: %w", i, err)
		}
		rawMtime, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob file %d mtime: %w", i, err)
		}
		if rawSize > math.MaxInt64 || rawMtime > math.MaxInt64 {
			return nil, 0, fmt.Errorf("store: unit blob file %d: size/mtime out of range", i)
		}
		files[i] = FileStat{Path: path, Size: int64(rawSize), ModTimeNanos: int64(rawMtime)}
	}
	return files, off, nil
}

// decodeNameEntries decodes count fixed-format NameEntry records starting
// at off.
func decodeNameEntries(b []byte, off, count int) ([]NameEntry, int, error) {
	names := make([]NameEntry, count)
	var err error
	for i := range names {
		var name string
		name, off, err = takeString(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob name %d: %w", i, err)
		}
		var idHash uint64
		idHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob name %d idHash: %w", i, err)
		}
		names[i] = NameEntry{Name: name, IDHash: idHash}
	}
	return names, off, nil
}

// decodeMethodEntries decodes the methodsCount-prefixed MethodSymbolEntry
// section starting at off.
func decodeMethodEntries(b []byte, off int) ([]MethodSymbolEntry, int, error) {
	methodCount, off, err := takeUint32(b, off)
	if err != nil {
		return nil, 0, fmt.Errorf("store: unit blob method count: %w", err)
	}
	methods := make([]MethodSymbolEntry, methodCount)
	for i := range methods {
		var name string
		name, off, err = takeString(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob method %d: %w", i, err)
		}
		var pkgHash, typeIDHash, methodPkgHash, methodIDHash, fingerprint uint64
		pkgHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob method %d pkgHash: %w", i, err)
		}
		typeIDHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob method %d typeIDHash: %w", i, err)
		}
		methodPkgHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob method %d methodPkgHash: %w", i, err)
		}
		methodIDHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob method %d methodIDHash: %w", i, err)
		}
		fingerprint, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob method %d fingerprint: %w", i, err)
		}
		methods[i] = MethodSymbolEntry{Name: name, Entry: MethodEntry{
			PkgHash:          pkgHash,
			TypeSymbolIDHash: typeIDHash,
			MethodPkgHash:    methodPkgHash,
			MethodIDHash:     methodIDHash,
			Fingerprint:      fingerprint,
		}}
	}
	return methods, off, nil
}

// decodeSymStrEntries decodes the symstrCount-prefixed SymStrEntry section
// starting at off.
func decodeSymStrEntries(b []byte, off int) ([]SymStrEntry, int, error) {
	symStrCount, off, err := takeUint32(b, off)
	if err != nil {
		return nil, 0, fmt.Errorf("store: unit blob symstr count: %w", err)
	}
	symStrs := make([]SymStrEntry, symStrCount)
	for i := range symStrs {
		var idHash uint64
		idHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob symstr %d idHash: %w", i, err)
		}
		var sid string
		sid, off, err = takeString(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob symstr %d: %w", i, err)
		}
		symStrs[i] = SymStrEntry{IDHash: idHash, SymbolID: sid}
	}
	return symStrs, off, nil
}

// decodePostingEntries decodes the postingsCount-prefixed PostingEntry
// section starting at off.
func decodePostingEntries(b []byte, off int) ([]PostingEntry, int, error) {
	count, off, err := takeUint32(b, off)
	if err != nil {
		return nil, 0, fmt.Errorf("store: unit blob posting count: %w", err)
	}
	postings := make([]PostingEntry, count)
	for i := range postings {
		var targetPkgHash, targetIDHash uint64
		targetPkgHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob posting %d targetPkgHash: %w", i, err)
		}
		targetIDHash, off, err = takeUint64(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob posting %d targetIDHash: %w", i, err)
		}
		var file string
		file, off, err = takeString(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob posting %d file: %w", i, err)
		}
		var line, col, endCol int
		line, off, err = takeUint32(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob posting %d line: %w", i, err)
		}
		col, off, err = takeUint32(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob posting %d col: %w", i, err)
		}
		endCol, off, err = takeUint32(b, off)
		if err != nil {
			return nil, 0, fmt.Errorf("store: unit blob posting %d endCol: %w", i, err)
		}
		lineU32, err := uint32Field(line, "unit blob posting line")
		if err != nil {
			return nil, 0, err
		}
		colU32, err := uint32Field(col, "unit blob posting col")
		if err != nil {
			return nil, 0, err
		}
		endColU32, err := uint32Field(endCol, "unit blob posting endCol")
		if err != nil {
			return nil, 0, err
		}
		postings[i] = PostingEntry{
			TargetPkgHash: targetPkgHash,
			TargetIDHash:  targetIDHash,
			File:          file,
			Line:          lineU32,
			Col:           colU32,
			EndCol:        endColU32,
		}
	}
	return postings, off, nil
}

func takeBytes(b []byte, off, n int) ([]byte, int, error) {
	if off+n > len(b) {
		return nil, 0, fmt.Errorf("truncated (want %d bytes at offset %d, have %d)", n, off, len(b))
	}
	return b[off : off+n], off + n, nil
}

func takeString(b []byte, off int) (string, int, error) {
	n, off, err := takeUint32(b, off)
	if err != nil {
		return "", 0, err
	}
	raw, off, err := takeBytes(b, off, n)
	if err != nil {
		return "", 0, err
	}
	return string(raw), off, nil
}

func takeUint32(b []byte, off int) (int, int, error) {
	if off+4 > len(b) {
		return 0, 0, fmt.Errorf("truncated uint32 at offset %d", off)
	}
	return int(binary.LittleEndian.Uint32(b[off:])), off + 4, nil
}

// uint32Field converts n -- a value takeUint32 already decoded from a
// stored uint32 field, so mathematically always in [0, math.MaxUint32] --
// back to uint32, erroring instead of silently truncating a negative or
// overflowing value the way a bare uint32(n) conversion would if that
// invariant were ever violated (e.g. a corrupted or hand-crafted blob).
// what identifies the field in the returned error.
func uint32Field(n int, what string) (uint32, error) {
	if n < 0 || n > math.MaxUint32 {
		return 0, fmt.Errorf("store: %s %d out of uint32 range", what, n)
	}
	return uint32(n), nil
}

func takeUint64(b []byte, off int) (uint64, int, error) {
	if off+8 > len(b) {
		return 0, 0, fmt.Errorf("truncated uint64 at offset %d", off)
	}
	return binary.LittleEndian.Uint64(b[off:]), off + 8, nil
}
