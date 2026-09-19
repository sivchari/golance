package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/xref"
)

// wipeFacts deletes every blob from idx's CAS (via GCAged with maxAge=0, an
// age-only sweep that ignores liveness -- see (*store.CAS).GCAged's doc),
// leaving every package's own store.UnitPointer recorded in db but its
// facts blob missing from the CAS: a genuine facts-read failure
// (store.ErrNotFound, wrapped) distinct from both a canceled context and
// resolveAt's own "no symbol at this position" miss (xref.ErrNoSymbolAt),
// for pinning handleReferences/handleImplementation's error classification.
func wipeFacts(t *testing.T, s *Server) {
	t.Helper()
	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("wipeFacts: no index loaded")
	}
	if _, err := idx.cas.GCAged(time.Now().Add(365*24*time.Hour), 0); err != nil {
		t.Fatalf("GCAged: %v", err)
	}
}

// TestHandleReferences_ErrorClassification pins handleReferences' three-way
// error classification: a canceled/timed-out context and a genuine
// facts-read failure both propagate as (nil, err) to the client instead of
// today's blanket "log and answer empty", while resolveAt's own "no symbol
// at this position" miss (xref.ErrNoSymbolAt) keeps that existing fallback
// behavior untouched.
func TestHandleReferences_ErrorClassification(t *testing.T) {
	t.Run("canceled context returns error", func(t *testing.T) {
		s, _, root := newTestServer(t)
		file := callhFile(t, root)
		pos := callhPos(t, file, "Add", 1)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		result, err := s.handleReferences(ctx, mustMarshal(t, &protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
			Context: protocol.ReferenceContext{IncludeDeclaration: false},
		}))
		if result != nil {
			t.Errorf("handleReferences(canceled) result = %#v, want nil", result)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("handleReferences(canceled) error = %v, want it to wrap context.Canceled", err)
		}
	})

	t.Run("genuine facts-read failure returns error", func(t *testing.T) {
		s, _, root := newTestServer(t)
		file := callhFile(t, root)
		pos := callhPos(t, file, "Add", 1)
		wipeFacts(t, s)

		result, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
			Context: protocol.ReferenceContext{IncludeDeclaration: false},
		}))
		if result != nil {
			t.Errorf("handleReferences(wiped facts) result = %#v, want nil", result)
		}
		if err == nil {
			t.Fatal("handleReferences(wiped facts) error = nil, want a genuine facts-read failure")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, xref.ErrNoSymbolAt) {
			t.Errorf("handleReferences(wiped facts) error = %v, want neither context.Canceled nor xref.ErrNoSymbolAt", err)
		}
	})

	t.Run("no symbol at this position falls back to empty success", func(t *testing.T) {
		s, _, root := newTestServer(t)
		file := callhFile(t, root)
		// The file's first line is a doc comment: no reference or
		// definition is ever recorded there.
		pos := protocol.Position{Line: 0, Character: 0}

		result, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
			Context: protocol.ReferenceContext{IncludeDeclaration: false},
		}))
		if err != nil {
			t.Fatalf("handleReferences(no symbol) error = %v, want nil (today's fallback-then-empty behavior)", err)
		}
		locs, ok := result.(protocol.LocationSlice)
		if !ok || len(locs) != 0 {
			t.Errorf("handleReferences(no symbol) result = %#v, want an empty LocationSlice", result)
		}
	})
}

// TestHandleImplementation_ErrorClassification is
// TestHandleReferences_ErrorClassification's handleImplementation
// counterpart.
func TestHandleImplementation_ErrorClassification(t *testing.T) {
	t.Run("canceled context returns error", func(t *testing.T) {
		s, _, root := newTestServer(t)
		file := callhFile(t, root)
		pos := callhPos(t, file, "Add", 1)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		result, err := s.handleImplementation(ctx, mustMarshal(t, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if result != nil {
			t.Errorf("handleImplementation(canceled) result = %#v, want nil", result)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("handleImplementation(canceled) error = %v, want it to wrap context.Canceled", err)
		}
	})

	t.Run("genuine facts-read failure returns error", func(t *testing.T) {
		s, _, root := newTestServer(t)
		file := callhFile(t, root)
		pos := callhPos(t, file, "Add", 1)
		wipeFacts(t, s)

		result, err := s.handleImplementation(context.Background(), mustMarshal(t, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if result != nil {
			t.Errorf("handleImplementation(wiped facts) result = %#v, want nil", result)
		}
		if err == nil {
			t.Fatal("handleImplementation(wiped facts) error = nil, want a genuine facts-read failure")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, xref.ErrNoSymbolAt) {
			t.Errorf("handleImplementation(wiped facts) error = %v, want neither context.Canceled nor xref.ErrNoSymbolAt", err)
		}
	})

	t.Run("no symbol at this position falls back to empty success", func(t *testing.T) {
		s, _, root := newTestServer(t)
		file := callhFile(t, root)
		pos := protocol.Position{Line: 0, Character: 0}

		result, err := s.handleImplementation(context.Background(), mustMarshal(t, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if err != nil {
			t.Fatalf("handleImplementation(no symbol) error = %v, want nil (today's fallback-then-empty behavior)", err)
		}
		locs, ok := result.(protocol.LocationSlice)
		if !ok || len(locs) != 0 {
			t.Errorf("handleImplementation(no symbol) result = %#v, want an empty LocationSlice", result)
		}
	})
}
