package store

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// View wraps a raw facts blob and answers queries by computing offsets into
// it, without decoding the blob into Go structs. See the package doc for
// the lifetime rules that apply to the underlying []byte.
type View struct {
	symTable  []byte
	refsTable []byte
	fileTable []byte
	strTable  []byte

	symbolCount int
	refsCount   int
	fileCount   int
}

// NewView parses a facts blob's header and returns a View over it. NewView
// itself only reads the fixed-size header (O(1)); it does not decode symbol,
// ref, or string data.
func NewView(data []byte) (*View, error) {
	if len(data) < headerSize {
		return nil, fmt.Errorf("store: blob too short: %d bytes, want at least %d", len(data), headerSize)
	}
	if string(data[offMagic:offMagic+blobMagicLen]) != blobMagic {
		return nil, fmt.Errorf("store: bad magic %q", data[offMagic:offMagic+blobMagicLen])
	}
	version := binary.LittleEndian.Uint16(data[offVersion:])
	if version != currentVersion {
		return nil, fmt.Errorf("store: unsupported blob version %d (want %d)", version, currentVersion)
	}

	symbolCount := binary.LittleEndian.Uint32(data[offSymbolCount:])
	refsCount := binary.LittleEndian.Uint32(data[offRefsCount:])
	fileCount := binary.LittleEndian.Uint32(data[offFileCount:])

	symOff := binary.LittleEndian.Uint64(data[offSymTblOff:])
	symLen := binary.LittleEndian.Uint64(data[offSymTblLen:])
	refsOff := binary.LittleEndian.Uint64(data[offRefsTblOff:])
	refsLen := binary.LittleEndian.Uint64(data[offRefsTblLen:])
	fileOff := binary.LittleEndian.Uint64(data[offFileTblOff:])
	fileLen := binary.LittleEndian.Uint64(data[offFileTblLen:])
	strOff := binary.LittleEndian.Uint64(data[offStrTblOff:])
	strLen := binary.LittleEndian.Uint64(data[offStrTblLen:])

	if symLen != uint64(symbolCount)*symbolRecordSize {
		return nil, fmt.Errorf("store: symbol table length %d does not match symbolCount %d", symLen, symbolCount)
	}
	if refsLen != uint64(refsCount)*refsRecordSize {
		return nil, fmt.Errorf("store: refs table length %d does not match refsCount %d", refsLen, refsCount)
	}
	if fileLen != uint64(fileCount)*fileRecordSize {
		return nil, fmt.Errorf("store: file table length %d does not match fileCount %d", fileLen, fileCount)
	}

	symTable, err := section(data, symOff, symLen, "symbol table")
	if err != nil {
		return nil, err
	}
	refsTable, err := section(data, refsOff, refsLen, "refs table")
	if err != nil {
		return nil, err
	}
	fileTable, err := section(data, fileOff, fileLen, "file table")
	if err != nil {
		return nil, err
	}
	strTable, err := section(data, strOff, strLen, "string table")
	if err != nil {
		return nil, err
	}

	return &View{
		symTable:    symTable,
		refsTable:   refsTable,
		fileTable:   fileTable,
		strTable:    strTable,
		symbolCount: int(symbolCount),
		refsCount:   int(refsCount),
		fileCount:   int(fileCount),
	}, nil
}

func section(data []byte, off, ln uint64, name string) ([]byte, error) {
	if off > uint64(len(data)) || ln > uint64(len(data))-off {
		return nil, fmt.Errorf("store: %s [%d:%d] out of bounds for blob of length %d", name, off, off+ln, len(data))
	}
	return data[off : off+ln], nil
}

// SymbolCount returns the number of symbol definitions in the blob.
func (v *View) SymbolCount() int { return v.symbolCount }

// RefsCount returns the number of reference records in the blob.
func (v *View) RefsCount() int { return v.refsCount }

// FileCount returns the number of files in the blob's file table.
func (v *View) FileCount() int { return v.fileCount }

// SymbolAt returns the symbol at index i, in the order [Builder.AddSymbol]
// was called. It runs in O(1): only the fixed-size record is addressed, not
// decoded.
func (v *View) SymbolAt(i int) (Symbol, error) {
	if i < 0 || i >= v.symbolCount {
		return Symbol{}, fmt.Errorf("store: symbol index %d out of range [0, %d)", i, v.symbolCount)
	}
	return v.symbolRecordAt(i), nil
}

func (v *View) symbolRecordAt(i int) Symbol {
	off := i * symbolRecordSize
	return Symbol{rec: v.symTable[off : off+symbolRecordSize], str: v.strTable}
}

// LookupSymbol scans the symbol table for a symbol whose IDHash equals
// idHash and returns it. Symbols are not sorted by IDHash (they keep
// declaration order for SymbolAt), so this is O(symbolCount) hash
// comparisons — cheap even for large packages since no string data is
// touched during the scan.
func (v *View) LookupSymbol(idHash uint64) (Symbol, bool) {
	for i := 0; i < v.symbolCount; i++ {
		s := v.symbolRecordAt(i)
		if s.IDHash() == idHash {
			return s, true
		}
	}
	return Symbol{}, false
}

// FileAt returns the file path at index i in the package's file table.
func (v *View) FileAt(i int) (string, error) {
	if i < 0 || i >= v.fileCount {
		return "", fmt.Errorf("store: file index %d out of range [0, %d)", i, v.fileCount)
	}
	off := i * fileRecordSize
	rec := v.fileTable[off : off+fileRecordSize]
	pathOff := binary.LittleEndian.Uint32(rec[fileOffPathOff:])
	pathLen := binary.LittleEndian.Uint32(rec[fileOffPathLen:])
	return string(v.strTable[pathOff : pathOff+pathLen]), nil
}

// RefAt returns the ref record at index i in position-sorted order.
func (v *View) RefAt(i int) (Ref, error) {
	if i < 0 || i >= v.refsCount {
		return Ref{}, fmt.Errorf("store: ref index %d out of range [0, %d)", i, v.refsCount)
	}
	return v.refRecordAt(i), nil
}

func (v *View) refRecordAt(i int) Ref {
	off := i * refsRecordSize
	return Ref{rec: v.refsTable[off : off+refsRecordSize]}
}

// RefsAt returns the reference whose position span at (fileIdx, line)
// contains col, if any. The refs table is sorted by (fileIdx, line, col),
// so this runs in O(log refsCount) via binary search plus a single
// containment check.
func (v *View) RefsAt(fileIdx, line, col uint32) (Ref, bool) {
	n := v.refsCount
	idx := sort.Search(n, func(i int) bool {
		return refKeyGreater(v.refRecordAt(i), fileIdx, line, col)
	}) - 1
	if idx < 0 {
		return Ref{}, false
	}
	r := v.refRecordAt(idx)
	if r.FileIdx() == fileIdx && r.Line() == line && col >= r.Col() && col < r.EndCol() {
		return r, true
	}
	return Ref{}, false
}

// refKeyGreater reports whether r's (FileIdx, Line, Col) key sorts strictly
// after (fileIdx, line, col).
func refKeyGreater(r Ref, fileIdx, line, col uint32) bool {
	if r.FileIdx() != fileIdx {
		return r.FileIdx() > fileIdx
	}
	if r.Line() != line {
		return r.Line() > line
	}
	return r.Col() > col
}

// RefsTo returns every reference in this package's blob whose
// ToSymbolIDHash equals idHash. It only finds references made from within
// this package; resolving references made from other packages is the
// caller's responsibility (combine with the import-closure of packages that
// could reference this symbol, then call RefsTo on each of their blobs).
func (v *View) RefsTo(idHash uint64) []Ref {
	var out []Ref
	for i := 0; i < v.refsCount; i++ {
		r := v.refRecordAt(i)
		if r.ToSymbolIDHash() == idHash {
			out = append(out, r)
		}
	}
	return out
}

// Symbol is a zero-copy view over one fixed-size symbol record plus the
// string table it references. Field accessors compute offsets on demand;
// none of them require decoding the whole record set.
type Symbol struct {
	rec []byte
	str []byte
}

// IDHash returns the symbol's SymbolID hash (see [Hash], [BuildSymbolID]).
func (s Symbol) IDHash() uint64 { return binary.LittleEndian.Uint64(s.rec[symOffIDHash:]) }

// Kind returns the caller-defined symbol kind byte.
func (s Symbol) Kind() uint8 { return s.rec[symOffKind] }

// Flags returns the caller-defined symbol flags byte.
func (s Symbol) Flags() uint8 { return s.rec[symOffFlags] }

// Name returns the symbol's name.
func (s Symbol) Name() string { return s.strField(symOffNameOff, symOffNameLen) }

// Doc returns the symbol's doc comment, or "" if it has none.
func (s Symbol) Doc() string { return s.strField(symOffDocOff, symOffDocLen) }

// Sig returns the symbol's signature string, or "" if it has none.
func (s Symbol) Sig() string { return s.strField(symOffSigOff, symOffSigLen) }

// FileIdx returns the index into the package's file table of the file this
// symbol is declared in.
func (s Symbol) FileIdx() uint32 { return binary.LittleEndian.Uint32(s.rec[symOffFileIdx:]) }

// Line returns the symbol's declaration line (1-based).
func (s Symbol) Line() uint32 { return binary.LittleEndian.Uint32(s.rec[symOffLine:]) }

// Col returns the symbol's declaration column (1-based).
func (s Symbol) Col() uint32 { return binary.LittleEndian.Uint32(s.rec[symOffCol:]) }

func (s Symbol) strField(offOff, lenOff int) string {
	off := binary.LittleEndian.Uint32(s.rec[offOff:])
	ln := binary.LittleEndian.Uint32(s.rec[lenOff:])
	return string(s.str[off : off+ln])
}

// Ref is a zero-copy view over one fixed-size reference record.
type Ref struct {
	rec []byte
}

// FileIdx returns the index into the package's file table of the file the
// reference occurs in.
func (r Ref) FileIdx() uint32 { return binary.LittleEndian.Uint32(r.rec[refOffFileIdx:]) }

// Line returns the reference's line (1-based).
func (r Ref) Line() uint32 { return binary.LittleEndian.Uint32(r.rec[refOffLine:]) }

// Col returns the reference span's start column (1-based).
func (r Ref) Col() uint32 { return binary.LittleEndian.Uint32(r.rec[refOffCol:]) }

// EndCol returns the reference span's end column, exclusive.
func (r Ref) EndCol() uint32 { return binary.LittleEndian.Uint32(r.rec[refOffEndCol:]) }

// ToSymbolIDHash returns the SymbolID hash of the symbol this reference
// resolves to.
func (r Ref) ToSymbolIDHash() uint64 {
	return binary.LittleEndian.Uint64(r.rec[refOffToSymbolIDHash:])
}

// ToPkgHash returns the package path hash of the symbol this reference
// resolves to.
func (r Ref) ToPkgHash() uint64 { return binary.LittleEndian.Uint64(r.rec[refOffToPkgHash:]) }
