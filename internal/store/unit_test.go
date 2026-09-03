package store

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestUnitBlobRoundTrip(t *testing.T) {
	want := UnitBlob{
		Facts:  []byte("facts-bytes"),
		Export: []byte("export-bytes"),
		Files: []FileStat{
			{Path: "a.go", Size: 10, ModTimeNanos: 111},
			{Path: "sub/b.go", Size: 20, ModTimeNanos: 222},
		},
		Index: PackageIndexEntries{
			Names: []NameEntry{
				{Name: "Foo", IDHash: 1},
				{Name: "Bar", IDHash: 2},
			},
			Methods: []MethodSymbolEntry{
				{Name: "String", Entry: MethodEntry{PkgHash: 10, TypeSymbolIDHash: 20, MethodPkgHash: 30, MethodIDHash: 40, Fingerprint: 50}},
			},
			SymStrs: []SymStrEntry{
				{IDHash: 1, SymbolID: "pkg#Foo"},
				{IDHash: 2, SymbolID: "pkg#Bar"},
			},
			Postings: []PostingEntry{
				{TargetPkgHash: 100, TargetIDHash: 200, File: "a.go", Line: 5, Col: 1, EndCol: 4},
			},
		},
	}

	encoded := EncodeUnitBlob(&want)
	got, err := DecodeUnitBlob(encoded)
	if err != nil {
		t.Fatalf("DecodeUnitBlob() error = %v", err)
	}

	assertBytesEqual(t, "Facts", got.Facts, want.Facts)
	assertBytesEqual(t, "Export", got.Export, want.Export)
	assertFilesEqual(t, got.Files, want.Files)
	assertIndexEqual(t, &got.Index, &want.Index)
}

// assertBytesEqual reports a t.Errorf for field if got and want differ.
func assertBytesEqual(t *testing.T, field string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

// assertFilesEqual reports a mismatch between got and want's FileStat
// entries.
func assertFilesEqual(t *testing.T, got, want []FileStat) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Files = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Files[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// assertIndexEqual reports a mismatch between got and want's
// PackageIndexEntries — one field group at a time, delegating SymStrs and
// Postings (both list-shaped, needing a length check plus a per-element
// loop) to their own helpers to keep this function's own complexity low.
func assertIndexEqual(t *testing.T, got, want *PackageIndexEntries) {
	t.Helper()
	if len(got.Names) != len(want.Names) || got.Names[0] != want.Names[0] {
		t.Errorf("Index.Names = %+v, want %+v", got.Names, want.Names)
	}
	if len(got.Methods) != len(want.Methods) || got.Methods[0] != want.Methods[0] {
		t.Errorf("Index.Methods = %+v, want %+v", got.Methods, want.Methods)
	}
	assertSymStrsEqual(t, got.SymStrs, want.SymStrs)
	assertPostingsEqual(t, got.Postings, want.Postings)
}

// assertSymStrsEqual reports a mismatch between got and want's SymStrEntry
// lists.
func assertSymStrsEqual(t *testing.T, got, want []SymStrEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Index.SymStrs = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Index.SymStrs[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// assertPostingsEqual reports a mismatch between got and want's
// PostingEntry lists.
func assertPostingsEqual(t *testing.T, got, want []PostingEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Index.Postings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Index.Postings[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUnitBlobEmpty(t *testing.T) {
	encoded := EncodeUnitBlob(&UnitBlob{})
	got, err := DecodeUnitBlob(encoded)
	if err != nil {
		t.Fatalf("DecodeUnitBlob() error = %v", err)
	}
	if len(got.Facts) != 0 || len(got.Export) != 0 || len(got.Files) != 0 {
		t.Errorf("DecodeUnitBlob(empty) = %+v, want all empty", got)
	}
}

// TestUnitFactsRange_MatchesDecodedFacts verifies UnitFactsRange's own
// (offset, length) range, applied directly to an encoded blob, slices out
// exactly the same bytes DecodeUnitBlob's full decode returns as Facts.
func TestUnitFactsRange_MatchesDecodedFacts(t *testing.T) {
	want := UnitBlob{Facts: []byte("facts-bytes"), Export: []byte("export-bytes-longer-than-facts")}
	encoded := EncodeUnitBlob(&want)

	off, ln, err := UnitFactsRange(encoded)
	if err != nil {
		t.Fatalf("UnitFactsRange() error = %v", err)
	}
	if got := encoded[off : off+ln]; !bytes.Equal(got, want.Facts) {
		t.Errorf("UnitFactsRange() range = %q, want %q", got, want.Facts)
	}

	decoded, err := DecodeUnitBlob(encoded)
	if err != nil {
		t.Fatalf("DecodeUnitBlob() error = %v", err)
	}
	if !bytes.Equal(decoded.Facts, encoded[off:off+ln]) {
		t.Error("UnitFactsRange()'s range disagrees with DecodeUnitBlob's own Facts")
	}
}

func TestUnitFactsRange_RejectsShortOrBadHeader(t *testing.T) {
	if _, _, err := UnitFactsRange([]byte("short")); err == nil {
		t.Error("UnitFactsRange(too short) = nil error, want an error")
	}
	encoded := EncodeUnitBlob(&UnitBlob{Facts: []byte("hello")})
	binary.LittleEndian.PutUint16(encoded[4:6], unitVersion-1)
	if _, _, err := UnitFactsRange(encoded); err == nil {
		t.Error("UnitFactsRange(old version) = nil error, want an error")
	}
}

func TestUnitBlobTruncatedErrors(t *testing.T) {
	encoded := EncodeUnitBlob(&UnitBlob{Facts: []byte("hello"), Files: []FileStat{{Path: "a.go", Size: 1, ModTimeNanos: 2}}})
	if _, err := DecodeUnitBlob(encoded[:len(encoded)-2]); err == nil {
		t.Error("DecodeUnitBlob(truncated) = nil error, want an error")
	}
	if _, err := DecodeUnitBlob([]byte("short")); err == nil {
		t.Error("DecodeUnitBlob(too short for header) = nil error, want an error")
	}
}

// TestUnitBlobRejectsOldVersion pins unitVersion's own bump (1 -> 2, for
// MethodEntry's three new fields — see store.go's schemaVersion doc): a
// blob stamped with the PRIOR version must be rejected outright rather than
// decoded against the new, wider per-method record layout, which would
// otherwise misinterpret a version-1 blob's bytes instead of erroring.
func TestUnitBlobRejectsOldVersion(t *testing.T) {
	encoded := EncodeUnitBlob(&UnitBlob{Facts: []byte("hello")})
	binary.LittleEndian.PutUint16(encoded[4:6], unitVersion-1)
	if _, err := DecodeUnitBlob(encoded); err == nil {
		t.Error("DecodeUnitBlob(old version) = nil error, want an error")
	}
}
