package store

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestBuilderRoundTrip(t *testing.T) {
	b := NewBuilder()
	b.SetFiles([]string{"a.go", "b.go"})
	b.AddSymbol(SymbolInput{
		IDHash: Hash("pkg#Foo"), Kind: 1, Flags: 0,
		Name: "Foo", Doc: "Foo does things.", Sig: "func Foo()",
		FileIdx: 0, Line: 10, Col: 6,
	})
	b.AddSymbol(SymbolInput{
		IDHash: Hash("pkg#Bar"), Kind: 2, Flags: 1,
		Name: "Bar", Doc: "", Sig: "func Bar() int",
		FileIdx: 1, Line: 3, Col: 6,
	})
	b.AddRef(RefInput{FileIdx: 0, Line: 12, Col: 2, EndCol: 5, ToSymbolIDHash: Hash("pkg#Bar"), ToPkgHash: Hash("pkg")})
	b.AddRef(RefInput{FileIdx: 0, Line: 11, Col: 1, EndCol: 4, ToSymbolIDHash: Hash("pkg#Foo"), ToPkgHash: Hash("pkg")})

	blob, err := b.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	v, err := NewView(blob)
	if err != nil {
		t.Fatalf("NewView() error = %v", err)
	}

	if got, want := v.SymbolCount(), 2; got != want {
		t.Fatalf("SymbolCount() = %d, want %d", got, want)
	}
	if got, want := v.RefsCount(), 2; got != want {
		t.Fatalf("RefsCount() = %d, want %d", got, want)
	}
	if got, want := v.FileCount(), 2; got != want {
		t.Fatalf("FileCount() = %d, want %d", got, want)
	}

	foo, err := v.SymbolAt(0)
	if err != nil {
		t.Fatalf("SymbolAt(0) error = %v", err)
	}
	if got, want := foo.Name(), "Foo"; got != want {
		t.Errorf("SymbolAt(0).Name() = %q, want %q", got, want)
	}
	if got, want := foo.Doc(), "Foo does things."; got != want {
		t.Errorf("SymbolAt(0).Doc() = %q, want %q", got, want)
	}
	if got, want := foo.Sig(), "func Foo()"; got != want {
		t.Errorf("SymbolAt(0).Sig() = %q, want %q", got, want)
	}
	if got, want := foo.Kind(), uint8(1); got != want {
		t.Errorf("SymbolAt(0).Kind() = %d, want %d", got, want)
	}
	if got, want := foo.Line(), uint32(10); got != want {
		t.Errorf("SymbolAt(0).Line() = %d, want %d", got, want)
	}

	bar, err := v.SymbolAt(1)
	if err != nil {
		t.Fatalf("SymbolAt(1) error = %v", err)
	}
	if got, want := bar.Doc(), ""; got != want {
		t.Errorf("SymbolAt(1).Doc() = %q, want empty", got)
	}

	sym, ok := v.LookupSymbol(Hash("pkg#Bar"))
	if !ok {
		t.Fatal("LookupSymbol(Bar) not found")
	}
	if got, want := sym.Name(), "Bar"; got != want {
		t.Errorf("LookupSymbol(Bar).Name() = %q, want %q", got, want)
	}

	if _, ok := v.LookupSymbol(Hash("pkg#DoesNotExist")); ok {
		t.Error("LookupSymbol(unknown) = found, want not found")
	}

	f0, err := v.FileAt(0)
	if err != nil || f0 != "a.go" {
		t.Errorf("FileAt(0) = %q, %v, want %q, nil", f0, err, "a.go")
	}

	// Refs table must come back sorted by (fileIdx, line, col) regardless of
	// insertion order.
	r0, err := v.RefAt(0)
	if err != nil {
		t.Fatalf("RefAt(0) error = %v", err)
	}
	if got, want := r0.Line(), uint32(11); got != want {
		t.Errorf("RefAt(0).Line() = %d, want %d (refs table not sorted)", got, want)
	}
	r1, err := v.RefAt(1)
	if err != nil {
		t.Fatalf("RefAt(1) error = %v", err)
	}
	if got, want := r1.Line(), uint32(12); got != want {
		t.Errorf("RefAt(1).Line() = %d, want %d", got, want)
	}
}

func TestViewRefsAt(t *testing.T) {
	b := NewBuilder()
	b.SetFiles([]string{"a.go"})
	b.AddRef(RefInput{FileIdx: 0, Line: 5, Col: 10, EndCol: 15, ToSymbolIDHash: 111, ToPkgHash: 1})
	b.AddRef(RefInput{FileIdx: 0, Line: 5, Col: 20, EndCol: 23, ToSymbolIDHash: 222, ToPkgHash: 1})
	b.AddRef(RefInput{FileIdx: 1, Line: 1, Col: 1, EndCol: 4, ToSymbolIDHash: 333, ToPkgHash: 2})
	blob, err := b.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	v, err := NewView(blob)
	if err != nil {
		t.Fatalf("NewView() error = %v", err)
	}

	tests := []struct {
		name               string
		fileIdx, line, col uint32
		wantFound          bool
		wantToSymbolIDHash uint64
	}{
		{"inside first span", 0, 5, 12, true, 111},
		{"start of second span", 0, 5, 20, true, 222},
		{"end of second span exclusive", 0, 5, 23, false, 0},
		{"between spans", 0, 5, 16, false, 0},
		{"different file", 1, 1, 2, true, 333},
		{"unknown line", 0, 6, 0, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := v.RefsAt(tt.fileIdx, tt.line, tt.col)
			if ok != tt.wantFound {
				t.Fatalf("RefsAt(%d,%d,%d) found = %v, want %v", tt.fileIdx, tt.line, tt.col, ok, tt.wantFound)
			}
			if ok && r.ToSymbolIDHash() != tt.wantToSymbolIDHash {
				t.Errorf("RefsAt(%d,%d,%d).ToSymbolIDHash() = %d, want %d", tt.fileIdx, tt.line, tt.col, r.ToSymbolIDHash(), tt.wantToSymbolIDHash)
			}
		})
	}
}

func TestViewRefsTo(t *testing.T) {
	b := NewBuilder()
	b.SetFiles([]string{"a.go"})
	b.AddRef(RefInput{FileIdx: 0, Line: 1, Col: 1, EndCol: 2, ToSymbolIDHash: 42, ToPkgHash: 9})
	b.AddRef(RefInput{FileIdx: 0, Line: 2, Col: 1, EndCol: 2, ToSymbolIDHash: 99, ToPkgHash: 9})
	b.AddRef(RefInput{FileIdx: 0, Line: 3, Col: 1, EndCol: 2, ToSymbolIDHash: 42, ToPkgHash: 9})
	blob, err := b.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	v, err := NewView(blob)
	if err != nil {
		t.Fatalf("NewView() error = %v", err)
	}

	refs := v.RefsTo(42)
	if len(refs) != 2 {
		t.Fatalf("RefsTo(42) returned %d refs, want 2", len(refs))
	}
	for _, r := range refs {
		if r.ToSymbolIDHash() != 42 {
			t.Errorf("RefsTo(42) returned ref with ToSymbolIDHash() = %d", r.ToSymbolIDHash())
		}
	}

	if refs := v.RefsTo(999); refs != nil {
		t.Errorf("RefsTo(999) = %v, want nil", refs)
	}
}

func TestBuilderEmptyPackage(t *testing.T) {
	blob, err := NewBuilder().Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	v, err := NewView(blob)
	if err != nil {
		t.Fatalf("NewView() error = %v", err)
	}
	if v.SymbolCount() != 0 || v.RefsCount() != 0 || v.FileCount() != 0 {
		t.Fatalf("empty package: counts = (%d,%d,%d), want (0,0,0)", v.SymbolCount(), v.RefsCount(), v.FileCount())
	}
	if _, err := v.SymbolAt(0); err == nil {
		t.Error("SymbolAt(0) on empty package: want error, got nil")
	}
	if _, ok := v.LookupSymbol(1); ok {
		t.Error("LookupSymbol on empty package: want not found")
	}
	if _, ok := v.RefsAt(0, 0, 0); ok {
		t.Error("RefsAt on empty package: want not found")
	}
}

func TestBuilderHugeString(t *testing.T) {
	huge := strings.Repeat("x", 4*1024*1024) // 4 MiB doc comment
	b := NewBuilder()
	b.SetFiles([]string{"a.go"})
	b.AddSymbol(SymbolInput{IDHash: 1, Name: "Big", Doc: huge, FileIdx: 0, Line: 1, Col: 1})

	blob, err := b.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	v, err := NewView(blob)
	if err != nil {
		t.Fatalf("NewView() error = %v", err)
	}
	sym, err := v.SymbolAt(0)
	if err != nil {
		t.Fatalf("SymbolAt(0) error = %v", err)
	}
	if got := sym.Doc(); got != huge {
		t.Errorf("SymbolAt(0).Doc() length = %d, want %d", len(got), len(huge))
	}
}

func TestBuilderStringInterning(t *testing.T) {
	b := NewBuilder()
	b.SetFiles([]string{"a.go"})
	sig := "func Shared()"
	b.AddSymbol(SymbolInput{IDHash: 1, Name: "A", Sig: sig, FileIdx: 0, Line: 1, Col: 1})
	b.AddSymbol(SymbolInput{IDHash: 2, Name: "B", Sig: sig, FileIdx: 0, Line: 2, Col: 1})

	blob, err := b.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	// The string table should hold "a.go" + "A" + "B" + sig exactly once
	// each: len(sig) must appear only once, not twice, if interning works.
	strTblLen := int(binary.LittleEndian.Uint64(blob[offStrTblLen:]))
	wantDeduped := len("a.go") + len("A") + len("B") + len(sig)
	if strTblLen != wantDeduped {
		t.Errorf("string table length = %d, want %d (sig interned once); a non-deduped table would be %d", strTblLen, wantDeduped, wantDeduped+len(sig))
	}
}

func TestNewViewRejectsMalformedBlob(t *testing.T) {
	valid, err := NewBuilder().Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	tests := []struct {
		name string
		data []byte
	}{
		{"too short", valid[:headerSize-1]},
		{"bad magic", func() []byte {
			b := append([]byte(nil), valid...)
			b[0] = 'X'
			return b
		}()},
		{"bad version", func() []byte {
			b := append([]byte(nil), valid...)
			b[offVersion] = 0xFF
			return b
		}()},
		{"truncated section", func() []byte {
			b := NewBuilder()
			b.SetFiles([]string{"a.go"})
			b.AddSymbol(SymbolInput{IDHash: 1, Name: "A", FileIdx: 0, Line: 1, Col: 1})
			blob, err := b.Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			return blob[:len(blob)-1]
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewView(tt.data); err == nil {
				t.Error("NewView() error = nil, want non-nil")
			}
		})
	}
}
