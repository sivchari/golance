package server

import (
	"context"
	"testing"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/rpc"
)

// TestHandleInitialized_SetsClientInitialized covers the ordering guarantee
// workspaceReadyRefreshes relies on: s.clientInitialized only becomes true
// once handleInitialized runs, i.e. once the client's own "initialized"
// notification has arrived — never before, since the LSP spec forbids any
// server-initiated request earlier than that.
func TestHandleInitialized_SetsClientInitialized(t *testing.T) {
	s := New(rpc.NewServer(), Options{Logger: newTestLogger(t)})
	if s.clientInitialized.Load() {
		t.Fatalf("clientInitialized = true before handleInitialized ran, want false")
	}
	if err := s.handleInitialized(context.Background(), nil); err != nil {
		t.Fatalf("handleInitialized: %v", err)
	}
	if !s.clientInitialized.Load() {
		t.Fatalf("clientInitialized = false after handleInitialized ran, want true")
	}
}

// TestClientSupportsInlayHintRefresh covers clientSupportsInlayHintRefresh's
// three "no" cases (missing workspace capabilities, missing inlayHint
// capabilities, and refreshSupport explicitly false) plus the "yes" case —
// the gate refreshInlayHints relies on to avoid sending a client a request
// it never declared support for.
func TestClientSupportsInlayHintRefresh(t *testing.T) {
	trueVal, falseVal := true, false

	tests := []struct {
		name string
		p    *protocol.InitializeParams
		want bool
	}{
		{"no workspace capabilities", &protocol.InitializeParams{}, false},
		{
			"workspace capabilities without inlayHint",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{}}},
			false,
		},
		{
			"refreshSupport explicitly false",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				InlayHint: &protocol.InlayHintWorkspaceClientCapabilities{RefreshSupport: &falseVal},
			}}},
			false,
		},
		{
			"refreshSupport true",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				InlayHint: &protocol.InlayHintWorkspaceClientCapabilities{RefreshSupport: &trueVal},
			}}},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientSupportsInlayHintRefresh(tt.p); got != tt.want {
				t.Errorf("clientSupportsInlayHintRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClientSupportsSemanticTokensRefresh mirrors
// TestClientSupportsInlayHintRefresh for
// clientSupportsSemanticTokensRefresh — the gate refreshSemanticTokens
// relies on to avoid sending a client a request it never declared support
// for.
func TestClientSupportsSemanticTokensRefresh(t *testing.T) {
	trueVal, falseVal := true, false

	tests := []struct {
		name string
		p    *protocol.InitializeParams
		want bool
	}{
		{"no workspace capabilities", &protocol.InitializeParams{}, false},
		{
			"workspace capabilities without semanticTokens",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{}}},
			false,
		},
		{
			"refreshSupport explicitly false",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				SemanticTokens: &protocol.SemanticTokensWorkspaceClientCapabilities{RefreshSupport: &falseVal},
			}}},
			false,
		},
		{
			"refreshSupport true",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				SemanticTokens: &protocol.SemanticTokensWorkspaceClientCapabilities{RefreshSupport: &trueVal},
			}}},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientSupportsSemanticTokensRefresh(tt.p); got != tt.want {
				t.Errorf("clientSupportsSemanticTokensRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}
