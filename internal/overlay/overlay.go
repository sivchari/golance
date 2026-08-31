// Package overlay tracks editor-side unsaved document content (didOpen/
// didChange/didSave/didClose) and merges it with disk content behind a
// single FileReader entry point, so every read path in the server sees the
// same combined view.
package overlay

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// ErrNotOpen is returned by DidChange and DidSave when the document has no
// tracked overlay, e.g. a didChange for a document without a preceding
// didOpen.
var ErrNotOpen = errors.New("overlay: document not open")

// overlayDoc holds one open document's editor-side content. text is
// replaced, never mutated in place, on every DidOpen/DidChange/DidSave: a
// []byte returned by ReadFile or Get remains valid even after a later edit.
type overlayDoc struct {
	version int32
	text    []byte
	hash    [sha256.Size]byte
}

// Overlay is the in-memory store of open documents' unsaved content. It
// implements FileReader by preferring the overlay over disk.
type Overlay struct {
	mu   sync.RWMutex
	docs map[uri.URI]*overlayDoc
}

// New returns an empty Overlay.
func New() *Overlay {
	return &Overlay{docs: make(map[uri.URI]*overlayDoc)}
}

// FileReader is the single entry point for reading file content: overlay
// content if the file is open in the editor, disk content otherwise.
type FileReader interface {
	ReadFile(path string) ([]byte, error)
}

var _ FileReader = (*Overlay)(nil)

// ReadFile returns the overlay content for path if it is open, otherwise
// reads it from disk. The returned slice must not be modified: it may alias
// the overlay's internal buffer.
func (o *Overlay) ReadFile(path string) ([]byte, error) {
	o.mu.RLock()
	doc, ok := o.docs[uri.File(path)]
	// doc.text must be read while still holding the lock: DidChange/DidOpen
	// mutate the same *overlayDoc's text field in place (see overlayDoc's
	// doc), rather than swapping in a new one, so reading it after
	// releasing the lock races a concurrent DidChange writing it.
	var text []byte
	if ok {
		text = doc.text
	}
	o.mu.RUnlock()
	if ok {
		return text, nil
	}
	return os.ReadFile(filepath.Clean(path))
}

// Get returns the tracked overlay content, version, and content hash for u,
// or ok=false if u is not currently open. The returned slice must not be
// modified: it may alias the overlay's internal buffer.
func (o *Overlay) Get(u uri.URI) (text []byte, version int32, hash [sha256.Size]byte, ok bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	doc, found := o.docs[u]
	if !found {
		return nil, 0, [sha256.Size]byte{}, false
	}
	return doc.text, doc.version, doc.hash, true
}

// OpenFilesInDir returns the filesystem paths of every currently open
// document whose directory is dir, in no particular order. Used to publish
// an empty textDocument/publishDiagnostics for a file that has no
// diagnostics this round but is still open in the editor — without it,
// nothing ever tells the client such a file is clean, since a
// zero-diagnostic recheck result has no entry to notify from.
func (o *Overlay) OpenFilesInDir(dir string) []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var out []string
	for u := range o.docs {
		path := u.FsPath()
		if filepath.Dir(path) == dir {
			out = append(out, path)
		}
	}
	return out
}

// DidOpen starts tracking p's document, replacing any existing overlay for
// the same URI.
func (o *Overlay) DidOpen(p *protocol.DidOpenTextDocumentParams) {
	text := []byte(p.TextDocument.Text)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.docs[p.TextDocument.URI] = &overlayDoc{
		version: p.TextDocument.Version,
		text:    text,
		hash:    sha256.Sum256(text),
	}
}

// DidChange applies p's content changes to the tracked overlay for
// p.TextDocument.URI. It returns ErrNotOpen if the document has no tracked
// overlay.
func (o *Overlay) DidChange(p *protocol.DidChangeTextDocumentParams) error {
	u := p.TextDocument.URI
	o.mu.Lock()
	defer o.mu.Unlock()
	doc, ok := o.docs[u]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOpen, u)
	}
	text, err := applyContentChanges(doc.text, p.ContentChanges)
	if err != nil {
		return fmt.Errorf("overlay: apply content changes for %s: %w", u, err)
	}
	doc.text = text
	doc.version = p.TextDocument.Version
	doc.hash = sha256.Sum256(text)
	return nil
}

// DidSave refreshes the tracked overlay for p.TextDocument.URI with p.Text,
// if the client included it. It returns ErrNotOpen if the document has no
// tracked overlay.
func (o *Overlay) DidSave(p *protocol.DidSaveTextDocumentParams) error {
	if p.Text == nil {
		return nil
	}
	u := p.TextDocument.URI
	text := []byte(*p.Text)
	o.mu.Lock()
	defer o.mu.Unlock()
	doc, ok := o.docs[u]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOpen, u)
	}
	doc.text = text
	doc.hash = sha256.Sum256(text)
	return nil
}

// DidClose stops tracking p's document. Closing a document that is not
// tracked is a no-op.
func (o *Overlay) DidClose(p *protocol.DidCloseTextDocumentParams) {
	o.mu.Lock()
	delete(o.docs, p.TextDocument.URI)
	o.mu.Unlock()
}
