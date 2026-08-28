package store

import (
	"bytes"
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
				{Name: "String", Entry: MethodEntry{PkgHash: 10, TypeSymbolIDHash: 20}},
			},
			SymStrs: []SymStrEntry{
				{IDHash: 1, SymbolID: "pkg#Foo"},
				{IDHash: 2, SymbolID: "pkg#Bar"},
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

func TestUnitBlobTruncatedErrors(t *testing.T) {
	encoded := EncodeUnitBlob(&UnitBlob{Facts: []byte("hello"), Files: []FileStat{{Path: "a.go", Size: 1, ModTimeNanos: 2}}})
	if _, err := DecodeUnitBlob(encoded[:len(encoded)-2]); err == nil {
		t.Error("DecodeUnitBlob(truncated) = nil error, want an error")
	}
	if _, err := DecodeUnitBlob([]byte("short")); err == nil {
		t.Error("DecodeUnitBlob(too short for header) = nil error, want an error")
	}
}
