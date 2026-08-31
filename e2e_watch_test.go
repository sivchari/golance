package golance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestE2E_DidChangeWatchedFilesPicksUpExternalChanges verifies that
// golance's own workspace/didChangeWatchedFiles handling (internal/server's
// watch.go/workspace.go) keeps a running session's facts index current when
// files change outside the editor entirely — the shape of a `git pull` or
// branch switch — without ever restarting the session. Two changes are made
// directly on disk (never through textDocument/didChange, so this cannot
// pass merely because of overlay/didSave handling), each followed by a
// synthetic workspace/didChangeWatchedFiles notification exactly as a
// real client's own file watcher would send:
//
//   - editing an existing, already-indexed file (reload=false: only the
//     facts index itself needs revalidating, not the import graph);
//   - adding a brand-new file to an existing package's directory
//     (reload=true: the import graph must be reloaded to even learn the
//     new file exists before the facts index can be revalidated against
//     it).
func TestE2E_DidChangeWatchedFilesPicksUpExternalChanges(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EModule(t)
	c := startClient(t, root)
	c.initialize(t, root)
	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t, e2eIndexBudget)

	t.Run("existing_file_edited_externally", func(t *testing.T) {
		checkE2EExternalEditToKnownFile(t, c, &locs)
	})

	t.Run("new_file_added_to_known_package", func(t *testing.T) {
		checkE2ENewFileInKnownPackage(t, c, &locs)
	})
}

// checkE2EExternalEditToKnownFile rewrites locs.utilFile on disk (outside
// any editor buffer) to add a new exported function, notifies the server as
// a real client's file watcher would, and waits for the new symbol to
// become findable via workspace/symbol — proof the facts index was
// revalidated and rebuilt for content the server never saw through
// textDocument/didChange or didSave.
func checkE2EExternalEditToKnownFile(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	edited := strings.Replace(locs.utilSrc, "func Sum(a, b int) int {\n\treturn a + b\n}\n",
		"func Sum(a, b int) int {\n\treturn a + b\n}\n\n// Diff subtracts two ints.\nfunc Diff(a, b int) int {\n\treturn a - b\n}\n", 1)
	if edited == locs.utilSrc {
		t.Fatal("test setup: replacement did not match locs.utilSrc")
	}
	if err := os.WriteFile(locs.utilFile, []byte(edited), 0o600); err != nil {
		t.Fatalf("externally edit %s: %v", locs.utilFile, err)
	}

	notifyWatchedFileChanged(t, c, locs.utilFile, protocol.FileChangeTypeChanged)
	waitForWorkspaceSymbol(t, c, "Diff", e2eIndexBudget)
}

// checkE2ENewFileInKnownPackage adds a brand-new file to lib/util's
// directory (already a known package, but this exact file is not), sends a
// Created event, and waits for its new exported function to become
// findable — proof the import graph itself was reloaded (a new file in an
// existing package changes that package's GoFiles list, which only a fresh
// `go list` can discover) and the facts index rebuilt against the result.
func checkE2ENewFileInKnownPackage(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	newFile := filepath.Join(filepath.Dir(locs.utilFile), "util_extra.go")
	const src = `package util

// Triple triples an int.
func Triple(n int) int {
	return n * 3
}
`
	if err := os.WriteFile(newFile, []byte(src), 0o600); err != nil {
		t.Fatalf("write %s: %v", newFile, err)
	}

	notifyWatchedFileChanged(t, c, newFile, protocol.FileChangeTypeCreated)
	waitForWorkspaceSymbol(t, c, "Triple", e2eIndexBudget)
}

// notifyWatchedFileChanged sends workspace/didChangeWatchedFiles for a
// single file, as a real client's file watcher would after a change made
// outside the editor.
func notifyWatchedFileChanged(t *testing.T, c *lspClient, path string, kind protocol.FileChangeType) {
	t.Helper()
	c.notify(t, protocol.MethodWorkspaceDidChangeWatchedFiles, &protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri.File(path), Type: kind}},
	})
}

// waitForWorkspaceSymbol polls workspace/symbol for query until it returns
// at least one result whose name is exactly query, or fails t after
// timeout. Unlike textDocument/definition or textDocument/references
// (protocol.LocationSlice, see waitForNonEmptyLocations),
// workspace/symbol's result is a protocol.SymbolInformationSlice, so this
// needs its own polling loop rather than reusing that one.
func waitForWorkspaceSymbol(t *testing.T, c *lspClient, query string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp := c.call(t, protocol.MethodWorkspaceSymbol, &protocol.WorkspaceSymbolParams{Query: query}, e2eRequestBudget)
		if len(resp.Error) == 0 {
			var syms protocol.SymbolInformationSlice
			if err := protocol.Unmarshal(resp.Result, &syms); err == nil {
				for _, s := range syms {
					if s.Name == query {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace/symbol never found %q within %s of the watched-file notification", query, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
