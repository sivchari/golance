package store

import (
	"encoding/binary"
	"fmt"
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
	unitMagic   = "GLUB"
	unitVersion = 1
)

// encodeUnitBlob serializes u as:
//
//	[4]magic [2]version [2]reserved
//	[4]factsLen [4]exportLen [4]fileCount [4]namesCount [4]methodsCount [4]symstrCount
//	[factsLen]facts [exportLen]export
//	fileCount * { [4]pathLen [pathLen]path [8]size [8]modTimeNanos }
//	namesCount * { [4]nameLen [nameLen]name [8]idHash }
//	methodsCount * { [4]nameLen [nameLen]name [8]pkgHash [8]typeSymbolIDHash }
//	symstrCount * { [8]idHash [4]symbolIDLen [symbolIDLen]symbolID }
func EncodeUnitBlob(u UnitBlob) []byte {
	size := 24 + 8 + len(u.Facts) + len(u.Export) // +8: methodsCount, symstrCount (see putIndexEntries)
	for _, f := range u.Files {
		size += 4 + len(f.Path) + 16
	}
	for _, n := range u.Index.Names {
		size += 4 + len(n.Name) + 8
	}
	for _, m := range u.Index.Methods {
		size += 4 + len(m.Name) + 16
	}
	for _, s := range u.Index.SymStrs {
		size += 8 + 4 + len(s.SymbolID)
	}

	b := make([]byte, size)
	copy(b[0:4], unitMagic)
	binary.LittleEndian.PutUint16(b[4:6], unitVersion)
	binary.LittleEndian.PutUint32(b[8:12], uint32(len(u.Facts)))
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(u.Export)))
	binary.LittleEndian.PutUint32(b[16:20], uint32(len(u.Files)))
	binary.LittleEndian.PutUint32(b[20:24], uint32(len(u.Index.Names)))
	off := 24
	off += copy(b[off:], u.Facts)
	off += copy(b[off:], u.Export)
	for _, f := range u.Files {
		binary.LittleEndian.PutUint32(b[off:], uint32(len(f.Path)))
		off += 4
		off += copy(b[off:], f.Path)
		binary.LittleEndian.PutUint64(b[off:], uint64(f.Size))
		off += 8
		binary.LittleEndian.PutUint64(b[off:], uint64(f.ModTimeNanos))
		off += 8
	}
	off = putIndexEntries(b, off, u.Index)
	return b[:off]
}

// putIndexEntries appends the methodsCount/symstrCount-prefixed sections
// following the fixed-size header's namesCount-and-earlier fields: the
// header only reserves space for namesCount because methods/symstrs counts
// are folded into the trailing sections themselves (see decodeUnitBlob).
func putIndexEntries(b []byte, off int, idx PackageIndexEntries) int {
	for _, n := range idx.Names {
		binary.LittleEndian.PutUint32(b[off:], uint32(len(n.Name)))
		off += 4
		off += copy(b[off:], n.Name)
		binary.LittleEndian.PutUint64(b[off:], n.IDHash)
		off += 8
	}
	binary.LittleEndian.PutUint32(b[off:], uint32(len(idx.Methods)))
	off += 4
	for _, m := range idx.Methods {
		binary.LittleEndian.PutUint32(b[off:], uint32(len(m.Name)))
		off += 4
		off += copy(b[off:], m.Name)
		binary.LittleEndian.PutUint64(b[off:], m.Entry.PkgHash)
		off += 8
		binary.LittleEndian.PutUint64(b[off:], m.Entry.TypeSymbolIDHash)
		off += 8
	}
	binary.LittleEndian.PutUint32(b[off:], uint32(len(idx.SymStrs)))
	off += 4
	for _, s := range idx.SymStrs {
		binary.LittleEndian.PutUint64(b[off:], s.IDHash)
		off += 8
		binary.LittleEndian.PutUint32(b[off:], uint32(len(s.SymbolID)))
		off += 4
		off += copy(b[off:], s.SymbolID)
	}
	return off
}

// decodeUnitBlob parses b as encodeUnitBlob produced it. Facts and Export
// alias b (no copy); callers that need them to outlive b's own lifetime
// (e.g. a []byte returned from [CAS.Get], safe to retain since it is a fresh
// os.ReadFile result, not a memory-mapped transaction view) may keep them as
// is.
func DecodeUnitBlob(b []byte) (UnitBlob, error) {
	if len(b) < 24 || string(b[0:4]) != unitMagic {
		return UnitBlob{}, fmt.Errorf("store: bad unit blob header")
	}
	version := binary.LittleEndian.Uint16(b[4:6])
	if version != unitVersion {
		return UnitBlob{}, fmt.Errorf("store: unsupported unit blob version %d (want %d)", version, unitVersion)
	}
	factsLen := int(binary.LittleEndian.Uint32(b[8:12]))
	exportLen := int(binary.LittleEndian.Uint32(b[12:16]))
	fileCount := int(binary.LittleEndian.Uint32(b[16:20]))
	nameCount := int(binary.LittleEndian.Uint32(b[20:24]))

	off := 24
	facts, off, err := takeBytes(b, off, factsLen)
	if err != nil {
		return UnitBlob{}, fmt.Errorf("store: unit blob facts: %w", err)
	}
	export, off, err := takeBytes(b, off, exportLen)
	if err != nil {
		return UnitBlob{}, fmt.Errorf("store: unit blob export: %w", err)
	}

	files := make([]FileStat, fileCount)
	for i := range files {
		var path string
		path, off, err = takeString(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob file %d: %w", i, err)
		}
		var size, mtime uint64
		size, off, err = takeUint64(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob file %d size: %w", i, err)
		}
		mtime, off, err = takeUint64(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob file %d mtime: %w", i, err)
		}
		files[i] = FileStat{Path: path, Size: int64(size), ModTimeNanos: int64(mtime)}
	}

	names := make([]NameEntry, nameCount)
	for i := range names {
		var name string
		name, off, err = takeString(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob name %d: %w", i, err)
		}
		var idHash uint64
		idHash, off, err = takeUint64(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob name %d idHash: %w", i, err)
		}
		names[i] = NameEntry{Name: name, IDHash: idHash}
	}

	methodCount, off, err := takeUint32(b, off)
	if err != nil {
		return UnitBlob{}, fmt.Errorf("store: unit blob method count: %w", err)
	}
	methods := make([]MethodSymbolEntry, methodCount)
	for i := range methods {
		var name string
		name, off, err = takeString(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob method %d: %w", i, err)
		}
		var pkgHash, typeIDHash uint64
		pkgHash, off, err = takeUint64(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob method %d pkgHash: %w", i, err)
		}
		typeIDHash, off, err = takeUint64(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob method %d typeIDHash: %w", i, err)
		}
		methods[i] = MethodSymbolEntry{Name: name, Entry: MethodEntry{PkgHash: pkgHash, TypeSymbolIDHash: typeIDHash}}
	}

	symStrCount, off, err := takeUint32(b, off)
	if err != nil {
		return UnitBlob{}, fmt.Errorf("store: unit blob symstr count: %w", err)
	}
	symStrs := make([]SymStrEntry, symStrCount)
	for i := range symStrs {
		var idHash uint64
		idHash, off, err = takeUint64(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob symstr %d idHash: %w", i, err)
		}
		var sid string
		sid, off, err = takeString(b, off)
		if err != nil {
			return UnitBlob{}, fmt.Errorf("store: unit blob symstr %d: %w", i, err)
		}
		symStrs[i] = SymStrEntry{IDHash: idHash, SymbolID: sid}
	}

	return UnitBlob{
		Facts:  facts,
		Export: export,
		Files:  files,
		Index:  PackageIndexEntries{Names: names, Methods: methods, SymStrs: symStrs},
	}, nil
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
	raw, off, err := takeBytes(b, off, int(n))
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

func takeUint64(b []byte, off int) (uint64, int, error) {
	if off+8 > len(b) {
		return 0, 0, fmt.Errorf("truncated uint64 at offset %d", off)
	}
	return binary.LittleEndian.Uint64(b[off:]), off + 8, nil
}
