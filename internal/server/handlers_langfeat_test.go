package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
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
