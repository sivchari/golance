package store

// Binary layout of a facts blob:
//
//	[header]        fixed size, see offXxx constants below
//	[symbol table]  symbolCount * symbolRecordSize, insertion order
//	[refs table]    refsCount * refsRecordSize, sorted by (fileIdx, line, col)
//	[file table]    fileCount * fileRecordSize
//	[string table]  raw interned bytes for names/docs/signatures/paths
//
// All multi-byte integers are little-endian. Offsets stored inside a symbol
// or file record are relative to the start of the string table; offsets
// stored in the header are relative to the start of the blob.

const (
	blobMagic        = "GLFB"
	blobMagicLen     = 4
	currentVersion   = 1
	symbolRecordSize = 48
	refsRecordSize   = 32
	fileRecordSize   = 8
)

// header field byte offsets.
const (
	offMagic       = 0  // [4]byte, ASCII blobMagic
	offVersion     = 4  // uint16
	offReserved1   = 6  // uint16, unused, must be zero
	offSymbolCount = 8  // uint32
	offRefsCount   = 12 // uint32
	offFileCount   = 16 // uint32
	offReserved2   = 20 // uint32, unused, must be zero
	offSymTblOff   = 24 // uint64
	offSymTblLen   = 32 // uint64
	offRefsTblOff  = 40 // uint64
	offRefsTblLen  = 48 // uint64
	offFileTblOff  = 56 // uint64
	offFileTblLen  = 64 // uint64
	offStrTblOff   = 72 // uint64
	offStrTblLen   = 80 // uint64
	headerSize     = 88
)

// symbol record field byte offsets (relative to the start of the record).
const (
	symOffIDHash  = 0  // uint64
	symOffKind    = 8  // uint8
	symOffFlags   = 9  // uint8
	symOffNameOff = 12 // uint32, relative to string table
	symOffNameLen = 16 // uint32
	symOffDocOff  = 20 // uint32, relative to string table
	symOffDocLen  = 24 // uint32
	symOffSigOff  = 28 // uint32, relative to string table
	symOffSigLen  = 32 // uint32
	symOffFileIdx = 36 // uint32
	symOffLine    = 40 // uint32
	symOffCol     = 44 // uint32
)

// refs record field byte offsets (relative to the start of the record).
const (
	refOffFileIdx        = 0  // uint32
	refOffLine           = 4  // uint32
	refOffCol            = 8  // uint32
	refOffEndCol         = 12 // uint32
	refOffToSymbolIDHash = 16 // uint64
	refOffToPkgHash      = 24 // uint64
)

// file record field byte offsets (relative to the start of the record).
const (
	fileOffPathOff = 0 // uint32, relative to string table
	fileOffPathLen = 4 // uint32
)
