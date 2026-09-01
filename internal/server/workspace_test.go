package server

import (
	"testing"

	"github.com/sivchari/golance/internal/rpc"
)

// TestWorkspaceReadyRefreshes covers workspaceReadyRefreshes' gating: no
// refresh call is ever returned before s.clientInitialized is set (the LSP
// spec forbids a server-initiated request before the client's "initialized"
// notification, and setWorkspace's first call happens synchronously inside
// handleInitialize, well before that arrives — see refreshOnWorkspaceReady's
// doc), and afterward exactly the capabilities the client declared are
// represented, one refresh call per capability.
func TestWorkspaceReadyRefreshes(t *testing.T) {
	tests := []struct {
		name               string
		clientInitialized  bool
		inlayHintSupport   bool
		semanticTokenSup   bool
		wantRefreshesCount int
	}{
		{"before initialized, no capabilities", false, false, false, 0},
		{"before initialized, both capabilities", false, true, true, 0},
		{"after initialized, no capabilities", true, false, false, 0},
		{"after initialized, inlay hints only", true, true, false, 1},
		{"after initialized, semantic tokens only", true, false, true, 1},
		{"after initialized, both capabilities", true, true, true, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(rpc.NewServer(), Options{Logger: newTestLogger(t)})
			s.clientInitialized.Store(tt.clientInitialized)
			s.inlayHintRefreshSupport.Store(tt.inlayHintSupport)
			s.semanticTokensRefreshSupport.Store(tt.semanticTokenSup)

			got := s.workspaceReadyRefreshes()
			if len(got) != tt.wantRefreshesCount {
				t.Errorf("workspaceReadyRefreshes() returned %d refreshes, want %d", len(got), tt.wantRefreshesCount)
			}
		})
	}
}
