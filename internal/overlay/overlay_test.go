package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestDidOpenThenReadFileReturnsOverlayContent(t *testing.T) {
	o := New()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: uri.File(path), LanguageID: "go", Version: 1, Text: "package overlay\n",
	}})

	got, err := o.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "package overlay\n" {
		t.Fatalf("ReadFile() = %q, want overlay content", got)
	}
}

func TestReadFileFallsBackToDiskWhenNotOpen(t *testing.T) {
	o := New()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := o.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "package disk\n" {
		t.Fatalf("ReadFile() = %q, want disk content", got)
	}
}

func TestReadFileMissingBothOverlayAndDiskReturnsError(t *testing.T) {
	o := New()
	if _, err := o.ReadFile(filepath.Join(t.TempDir(), "missing.go")); err == nil {
		t.Fatal("ReadFile() error = nil, want error")
	}
}

func TestDidChangeAppliesIncrementalEdit(t *testing.T) {
	o := New()
	u := uri.File("/virtual/a.go")
	o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: u, LanguageID: "go", Version: 1, Text: "hello world",
	}})

	err := o.DidChange(&protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u},
			Version:                2,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangePartial{
				Range: protocol.Range{
					Start: protocol.Position{Line: 0, Character: 5},
					End:   protocol.Position{Line: 0, Character: 11},
				},
				Text: ", Go",
			},
		},
	})
	if err != nil {
		t.Fatalf("DidChange() error = %v", err)
	}

	text, version, _, ok := o.Get(u)
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	if string(text) != "hello, Go" {
		t.Fatalf("text = %q, want %q", text, "hello, Go")
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}
}

func TestDidChangeWithoutDidOpenReturnsErrNotOpen(t *testing.T) {
	o := New()
	u := uri.File("/virtual/never-opened.go")
	err := o.DidChange(&protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u},
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: "x"},
		},
	})
	if !errors.Is(err, ErrNotOpen) {
		t.Fatalf("DidChange() error = %v, want ErrNotOpen", err)
	}
}

func TestDidChangeWithInvalidRangeReturnsError(t *testing.T) {
	o := New()
	u := uri.File("/virtual/a.go")
	o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: u, LanguageID: "go", Version: 1, Text: "short",
	}})
	err := o.DidChange(&protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u},
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangePartial{
				Range: protocol.Range{
					Start: protocol.Position{Line: 9, Character: 0},
					End:   protocol.Position{Line: 9, Character: 0},
				},
				Text: "x",
			},
		},
	})
	if err == nil {
		t.Fatal("DidChange() error = nil, want error")
	}
	// The overlay content must be left untouched by a failed change.
	text, _, _, _ := o.Get(u)
	if string(text) != "short" {
		t.Fatalf("text after failed change = %q, want unchanged %q", text, "short")
	}
}

func TestDidSaveRefreshesTextWhenIncluded(t *testing.T) {
	o := New()
	u := uri.File("/virtual/a.go")
	o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: u, LanguageID: "go", Version: 1, Text: "before",
	}})
	saved := "after"
	if err := o.DidSave(&protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: u},
		Text:         &saved,
	}); err != nil {
		t.Fatalf("DidSave() error = %v", err)
	}
	text, _, _, _ := o.Get(u)
	if string(text) != "after" {
		t.Fatalf("text = %q, want %q", text, "after")
	}
}

func TestDidSaveWithoutTextIsNoop(t *testing.T) {
	o := New()
	u := uri.File("/virtual/a.go")
	o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: u, LanguageID: "go", Version: 1, Text: "unchanged",
	}})
	if err := o.DidSave(&protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: u},
	}); err != nil {
		t.Fatalf("DidSave() error = %v", err)
	}
	text, _, _, _ := o.Get(u)
	if string(text) != "unchanged" {
		t.Fatalf("text = %q, want %q", text, "unchanged")
	}
}

func TestDidCloseRemovesOverlay(t *testing.T) {
	o := New()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := uri.File(path)
	o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: u, LanguageID: "go", Version: 1, Text: "overlay\n",
	}})
	o.DidClose(&protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: u}})

	if _, _, _, ok := o.Get(u); ok {
		t.Fatal("Get() ok = true after DidClose, want false")
	}
	got, err := o.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "disk\n" {
		t.Fatalf("ReadFile() after close = %q, want disk content", got)
	}
}

func TestDidCloseUntrackedDocumentIsNoop(t *testing.T) {
	o := New()
	o.DidClose(&protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File("/virtual/never-opened.go")},
	})
}

func TestOpenFilesInDirReturnsOnlyFilesInThatDir(t *testing.T) {
	o := New()
	a := uri.File("/virtual/pkg/a.go")
	b := uri.File("/virtual/pkg/b.go")
	c := uri.File("/virtual/other/c.go")
	for _, u := range []uri.URI{a, b, c} {
		o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
			URI: u, LanguageID: "go", Version: 1, Text: "package p\n",
		}})
	}

	got := o.OpenFilesInDir("/virtual/pkg")
	want := map[string]bool{"/virtual/pkg/a.go": true, "/virtual/pkg/b.go": true}
	if len(got) != len(want) {
		t.Fatalf("OpenFilesInDir() = %v, want %v", got, want)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("OpenFilesInDir() returned unexpected file %q", f)
		}
	}
}

func TestOpenFilesInDirEmptyForUnopenedDir(t *testing.T) {
	o := New()
	if got := o.OpenFilesInDir("/virtual/nothing-open-here"); len(got) != 0 {
		t.Errorf("OpenFilesInDir() = %v, want empty", got)
	}
}

func TestHashChangesWithContent(t *testing.T) {
	o := New()
	u := uri.File("/virtual/a.go")
	o.DidOpen(&protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: u, LanguageID: "go", Version: 1, Text: "one",
	}})
	_, _, hash1, _ := o.Get(u)

	if err := o.DidChange(&protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u}, Version: 2},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: "two"},
		},
	}); err != nil {
		t.Fatalf("DidChange() error = %v", err)
	}
	_, _, hash2, _ := o.Get(u)

	if hash1 == hash2 {
		t.Fatal("hash unchanged after content changed")
	}
}
