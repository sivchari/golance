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

	if !bytes.Equal(got.Facts, want.Facts) {
		t.Errorf("Facts = %q, want %q", got.Facts, want.Facts)
	}
	if !bytes.Equal(got.Export, want.Export) {
		t.Errorf("Export = %q, want %q", got.Export, want.Export)
	}
	if len(got.Files) != len(want.Files) {
		t.Fatalf("Files = %+v, want %+v", got.Files, want.Files)
	}
	for i := range want.Files {
		if got.Files[i] != want.Files[i] {
			t.Errorf("Files[%d] = %+v, want %+v", i, got.Files[i], want.Files[i])
		}
	}
	if len(got.Index.Names) != len(want.Index.Names) || got.Index.Names[0] != want.Index.Names[0] {
		t.Errorf("Index.Names = %+v, want %+v", got.Index.Names, want.Index.Names)
	}
	if len(got.Index.Methods) != len(want.Index.Methods) || got.Index.Methods[0] != want.Index.Methods[0] {
		t.Errorf("Index.Methods = %+v, want %+v", got.Index.Methods, want.Index.Methods)
	}
	if len(got.Index.SymStrs) != len(want.Index.SymStrs) {
		t.Fatalf("Index.SymStrs = %+v, want %+v", got.Index.SymStrs, want.Index.SymStrs)
	}
	for i := range want.Index.SymStrs {
		if got.Index.SymStrs[i] != want.Index.SymStrs[i] {
			t.Errorf("Index.SymStrs[%d] = %+v, want %+v", i, got.Index.SymStrs[i], want.Index.SymStrs[i])
		}
	}
	if len(got.Index.Postings) != len(want.Index.Postings) {
		t.Fatalf("Index.Postings = %+v, want %+v", got.Index.Postings, want.Index.Postings)
	}
	for i := range want.Index.Postings {
		if got.Index.Postings[i] != want.Index.Postings[i] {
			t.Errorf("Index.Postings[%d] = %+v, want %+v", i, got.Index.Postings[i], want.Index.Postings[i])
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
