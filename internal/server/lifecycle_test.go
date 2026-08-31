package server

import (
	"testing"

	"go.lsp.dev/protocol"
)

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
