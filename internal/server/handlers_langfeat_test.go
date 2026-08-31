package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestHintsEnabled_DefaultAllKindsOn checks that hintsEnabled reports every
// hint kind on before setHintsEnabled has ever run, golance's out-of-the-box
// default.
func TestHintsEnabled_DefaultAllKindsOn(t *testing.T) {
	s, _, _ := newTestServer(t)

	enabled := s.hintsEnabled()
	for _, k := range langfeat.AllHintKinds {
		if !enabled[k] {
			t.Errorf("hintsEnabled()[%s] = false, want true (default before any config is set)", k)
		}
	}
}

// TestHandleDidChangeConfiguration_UpdatesHintsEnabled checks that a
// workspace/didChangeConfiguration notification live-updates which inlay
// hint kinds are enabled, leaving kinds the settings did not mention alone.
func TestHandleDidChangeConfiguration_UpdatesHintsEnabled(t *testing.T) {
	s, _, _ := newTestServer(t)

	params := json.RawMessage(`{"settings":{"hints":{"parameterNames":false}}}`)
	if err := s.handleDidChangeConfiguration(context.Background(), params); err != nil {
		t.Fatalf("handleDidChangeConfiguration: %v", err)
	}

	enabled := s.hintsEnabled()
	if enabled[langfeat.ParameterNames] {
		t.Errorf("hintsEnabled()[parameterNames] = true, want false after didChangeConfiguration disabled it")
	}
	if !enabled[langfeat.ConstantValues] {
		t.Errorf("hintsEnabled()[constantValues] = false, want true (kind not mentioned in settings stays enabled)")
	}
}

// TestHandleDidChangeConfiguration_MalformedSettingsResetsToAllEnabled
// checks that settings golance cannot parse as the hints shape are treated
// as absent (every kind enabled), matching parseHintsSettings' documented
// fallback, rather than as a request failure.
func TestHandleDidChangeConfiguration_MalformedSettingsResetsToAllEnabled(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.setHintsEnabled(map[langfeat.HintKind]bool{langfeat.ParameterNames: false})

	params := json.RawMessage(`{"settings":"not an object"}`)
	if err := s.handleDidChangeConfiguration(context.Background(), params); err != nil {
		t.Fatalf("handleDidChangeConfiguration: %v", err)
	}

	enabled := s.hintsEnabled()
	if !enabled[langfeat.ParameterNames] {
		t.Errorf("hintsEnabled()[parameterNames] = false, want true (malformed settings resets to every kind enabled)")
	}
}

// TestHandleDidChangeConfiguration_InvalidParamsReturnsError checks that
// params golance cannot unmarshal at all is reported as an error, unlike a
// merely-malformed hints shape.
func TestHandleDidChangeConfiguration_InvalidParamsReturnsError(t *testing.T) {
	s, _, _ := newTestServer(t)

	if err := s.handleDidChangeConfiguration(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Fatal("handleDidChangeConfiguration(invalid JSON) = nil error, want non-nil")
	}
}

// unknownPackagePath returns a path inside root that check.Engine.Get
// cannot resolve to any loaded package (see check.GraphSource.PackageForFile):
// the file is never written to disk or opened in the overlay, so it stands
// in for a brand-new, unsaved file the graph hasn't picked up yet, or a
// build-tag-excluded/testdata file — the deterministic triggers Finding 1
// describes for check.Engine.Get's "not part of a known package" error.
func unknownPackagePath(root string) string {
	return filepath.Join(root, "greet", "unsaved.go")
}

// TestHandleHover_UnknownPackageReturnsNilResult checks that hover against a
// file check.Engine.Get cannot resolve to a package degrades to a null
// result, matching the LSP spec's "nothing to report" convention, instead of
// leaking Engine.Get's internal "is not part of a known package" error to
// the client as a wire error.
func TestHandleHover_UnknownPackageReturnsNilResult(t *testing.T) {
	s, _, root := newTestServer(t)

	result, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(unknownPackagePath(root))},
			Position:     protocol.Position{Line: 0, Character: 0},
		},
	}))
	if err != nil {
		t.Fatalf("handleHover(unknown package) error = %v, want nil (a null result, not a wire error)", err)
	}
	if result != nil {
		t.Fatalf("handleHover(unknown package) result = %#v, want nil", result)
	}
}

// TestHandleCompletion_UnknownPackageReturnsEmptyResult checks the same
// degradation as TestHandleHover_UnknownPackageReturnsNilResult for
// completion, which fires on nearly every keystroke and so hits this path
// most often in practice.
func TestHandleCompletion_UnknownPackageReturnsEmptyResult(t *testing.T) {
	s, _, root := newTestServer(t)

	result, err := s.handleCompletion(context.Background(), mustMarshal(t, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(unknownPackagePath(root))},
			Position:     protocol.Position{Line: 0, Character: 0},
		},
	}))
	if err != nil {
		t.Fatalf("handleCompletion(unknown package) error = %v, want nil (an empty result, not a wire error)", err)
	}
	items, ok := result.(protocol.CompletionItemSlice)
	if !ok || len(items) != 0 {
		t.Fatalf("handleCompletion(unknown package) result = %#v, want an empty protocol.CompletionItemSlice", result)
	}
}
