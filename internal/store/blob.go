package store

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// SymbolInput describes one symbol definition to add to a facts blob via
// [Builder.AddSymbol]. Kind and Flags are opaque to store: callers define
// and interpret their own encoding.
type SymbolInput struct {
	IDHash  uint64
	Kind    uint8
	Flags   uint8
	Name    string
	Doc     string
	Sig     string
	FileIdx uint32
	Line    uint32
	Col     uint32
}

// RefInput describes one reference (identifier occurrence) to add to a facts
// blob via [Builder.AddRef]. ToPkgHash is the hash of the referenced
// symbol's package path; it may equal the current package's own hash for an
// intra-package reference.
type RefInput struct {
	FileIdx        uint32
	Line           uint32
	Col            uint32
	EndCol         uint32
	ToSymbolIDHash uint64
	ToPkgHash      uint64
}

// Builder assembles a single package's facts blob from its symbols, refs,
// and file list. The zero value is not usable; construct with [NewBuilder].
type Builder struct {
	symbols []SymbolInput
	refs    []RefInput
	files   []string
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{}
}

// AddSymbol appends a symbol definition. Symbols are kept in the order
// added; [View.SymbolAt] enumerates them in that same order.
func (b *Builder) AddSymbol(s *SymbolInput) {
	b.symbols = append(b.symbols, *s)
}

// AddRef appends a reference. Build sorts refs by (FileIdx, Line, Col)
// before encoding, so callers may add them in any order.
func (b *Builder) AddRef(r RefInput) {
	b.refs = append(b.refs, r)
}

// SetFiles sets the package's file list. [SymbolInput.FileIdx] and
// [RefInput.FileIdx] index into this slice.
func (b *Builder) SetFiles(files []string) {
	b.files = files
}

// Build encodes the accumulated symbols, refs, and files into a facts blob.
// It returns an error if any table would exceed the blob format's uint32
// offset/length limits.
func (b *Builder) Build() ([]byte, error) {
	refs := make([]RefInput, len(b.refs))
	copy(refs, b.refs)
	sort.SliceStable(refs, func(i, j int) bool {
		return refLess(refs[i], refs[j])
	})

	nSymbols := len(b.symbols)
	if nSymbols > math.MaxUint32 {
		return nil, fmt.Errorf("store: too many symbols: %d", nSymbols)
	}
	nRefs := len(refs)
	if nRefs > math.MaxUint32 {
		return nil, fmt.Errorf("store: too many refs: %d", nRefs)
	}
	nFiles := len(b.files)
	if nFiles > math.MaxUint32 {
		return nil, fmt.Errorf("store: too many files: %d", nFiles)
	}

	it := newInterner()
	fileTable, err := buildFileTable(b.files, it)
	if err != nil {
		return nil, err
	}
	symTable, err := buildSymTable(b.symbols, it)
	if err != nil {
		return nil, err
	}
	refsTable := buildRefsTable(refs)

	symOff := uint64(headerSize)
	refsOff := symOff + uint64(len(symTable))
	fileOff := refsOff + uint64(len(refsTable))
	strOff := fileOff + uint64(len(fileTable))
	total := strOff + uint64(len(it.table))

	blob := make([]byte, total)
	copy(blob[offMagic:offMagic+blobMagicLen], blobMagic)
	binary.LittleEndian.PutUint16(blob[offVersion:], currentVersion)
	binary.LittleEndian.PutUint32(blob[offSymbolCount:], uint32(nSymbols))
	binary.LittleEndian.PutUint32(blob[offRefsCount:], uint32(nRefs))
	binary.LittleEndian.PutUint32(blob[offFileCount:], uint32(nFiles))
	binary.LittleEndian.PutUint64(blob[offSymTblOff:], symOff)
	binary.LittleEndian.PutUint64(blob[offSymTblLen:], uint64(len(symTable)))
	binary.LittleEndian.PutUint64(blob[offRefsTblOff:], refsOff)
	binary.LittleEndian.PutUint64(blob[offRefsTblLen:], uint64(len(refsTable)))
	binary.LittleEndian.PutUint64(blob[offFileTblOff:], fileOff)
	binary.LittleEndian.PutUint64(blob[offFileTblLen:], uint64(len(fileTable)))
	binary.LittleEndian.PutUint64(blob[offStrTblOff:], strOff)
	binary.LittleEndian.PutUint64(blob[offStrTblLen:], uint64(len(it.table)))

	copy(blob[symOff:], symTable)
	copy(blob[refsOff:], refsTable)
	copy(blob[fileOff:], fileTable)
	copy(blob[strOff:], it.table)

	return blob, nil
}

// interner deduplicates strings into one shared table, returning each
// string's (offset, length) the first time it's seen and reusing that
// offset for repeats.
type interner struct {
	table []byte
	seen  map[string]uint32
}

func newInterner() *interner {
	return &interner{table: make([]byte, 0, 4096), seen: make(map[string]uint32)}
}

func (it *interner) intern(s string) (off, ln uint32, err error) {
	if s == "" {
		return 0, 0, nil
	}
	n := len(s)
	if uint64(n) > math.MaxUint32 {
		return 0, 0, fmt.Errorf("store: interned string exceeds %d bytes", uint32(math.MaxUint32))
	}
	if off, ok := it.seen[s]; ok {
		return off, uint32(n), nil
	}
	curLen := len(it.table)
	if uint64(curLen)+uint64(n) > math.MaxUint32 {
		return 0, 0, fmt.Errorf("store: string table exceeds %d bytes", uint32(math.MaxUint32))
	}
	// curLen alone is already bounded by the compound check above (n >=
	// 0), spelled out as its own guard so the conversion below is
	// provably safe on its own, not just as a consequence of it.
	if curLen > math.MaxUint32 {
		return 0, 0, fmt.Errorf("store: string table exceeds %d bytes", uint32(math.MaxUint32))
	}
	off = uint32(curLen)
	it.table = append(it.table, s...)
	it.seen[s] = off
	return off, uint32(n), nil
}

// buildFileTable encodes files' interned paths into the fixed-width file
// table.
func buildFileTable(files []string, it *interner) ([]byte, error) {
	fileTable := make([]byte, len(files)*fileRecordSize)
	for i, f := range files {
		off, ln, err := it.intern(f)
		if err != nil {
			return nil, err
		}
		rec := fileTable[i*fileRecordSize : (i+1)*fileRecordSize]
		binary.LittleEndian.PutUint32(rec[fileOffPathOff:], off)
		binary.LittleEndian.PutUint32(rec[fileOffPathLen:], ln)
	}
	return fileTable, nil
}

// buildSymTable encodes symbols (with their Name/Doc/Sig interned) into the
// fixed-width symbol table.
func buildSymTable(symbols []SymbolInput, it *interner) ([]byte, error) {
	symTable := make([]byte, len(symbols)*symbolRecordSize)
	for i, s := range symbols {
		nameOff, nameLen, err := it.intern(s.Name)
		if err != nil {
			return nil, err
		}
		docOff, docLen, err := it.intern(s.Doc)
		if err != nil {
			return nil, err
		}
		sigOff, sigLen, err := it.intern(s.Sig)
		if err != nil {
			return nil, err
		}
		rec := symTable[i*symbolRecordSize : (i+1)*symbolRecordSize]
		binary.LittleEndian.PutUint64(rec[symOffIDHash:], s.IDHash)
		rec[symOffKind] = s.Kind
		rec[symOffFlags] = s.Flags
		binary.LittleEndian.PutUint32(rec[symOffNameOff:], nameOff)
		binary.LittleEndian.PutUint32(rec[symOffNameLen:], nameLen)
		binary.LittleEndian.PutUint32(rec[symOffDocOff:], docOff)
		binary.LittleEndian.PutUint32(rec[symOffDocLen:], docLen)
		binary.LittleEndian.PutUint32(rec[symOffSigOff:], sigOff)
		binary.LittleEndian.PutUint32(rec[symOffSigLen:], sigLen)
		binary.LittleEndian.PutUint32(rec[symOffFileIdx:], s.FileIdx)
		binary.LittleEndian.PutUint32(rec[symOffLine:], s.Line)
		binary.LittleEndian.PutUint32(rec[symOffCol:], s.Col)
	}
	return symTable, nil
}

// buildRefsTable encodes refs (already sorted by (FileIdx, Line, Col)) into
// the fixed-width refs table.
func buildRefsTable(refs []RefInput) []byte {
	refsTable := make([]byte, len(refs)*refsRecordSize)
	for i, r := range refs {
		rec := refsTable[i*refsRecordSize : (i+1)*refsRecordSize]
		binary.LittleEndian.PutUint32(rec[refOffFileIdx:], r.FileIdx)
		binary.LittleEndian.PutUint32(rec[refOffLine:], r.Line)
		binary.LittleEndian.PutUint32(rec[refOffCol:], r.Col)
		binary.LittleEndian.PutUint32(rec[refOffEndCol:], r.EndCol)
		binary.LittleEndian.PutUint64(rec[refOffToSymbolIDHash:], r.ToSymbolIDHash)
		binary.LittleEndian.PutUint64(rec[refOffToPkgHash:], r.ToPkgHash)
	}
	return refsTable
}

// refLess reports whether a sorts before b by (FileIdx, Line, Col).
func refLess(a, b RefInput) bool {
	if a.FileIdx != b.FileIdx {
		return a.FileIdx < b.FileIdx
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Col < b.Col
}
