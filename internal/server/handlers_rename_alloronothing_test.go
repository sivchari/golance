package server

// This file regression-tests a fix to a high-severity finding
// (audit-silent-failures.md's H4): handleRename used to skip an individual
// edit with a bare `continue` when correctResultRange could not resolve its
// range, then returned the remaining edits as a successful WorkspaceEdit --
// applying the rename to some references and silently leaving others under
// the old name, with no indication anything went wrong. A rename must be
// all-or-nothing: if any reference's range cannot be resolved, handleRename
// must now refuse the whole rename with an error, matching the same
// all-or-nothing contract TestHandleRename_RefusesLoudlyOnDirtyBuffer
// already pins for the unsaved-edits case.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/rpc"
)

// TestHandleRename_RefusesWhollyWhenAReferenceRangeUnresolvable reproduces
// H4's exact mechanism without an open/dirty overlay (that path is already
// refused earlier, by dirtyRenameFiles): a reference file the facts index
// has an occurrence recorded for is modified on disk directly -- never
// opened through didOpen/didChange, so dirtyLines reports it as not open,
// and handleRename proceeds past the dirty-buffer guard -- in a way that
// shifts the recorded call-site occurrence off its old column entirely
// (the new line is far shorter than the old occurrence's column), so
// correctResultRange's byteOffsetForLineCol fails for that one edit while
// the declaration's own edit, on an untouched line, still resolves fine.
// Before the fix, handleRename would have returned a WorkspaceEdit
// containing only the declaration's edit, silently omitting the call
// site. After the fix, it must refuse the whole rename instead.
func TestHandleRename_RefusesWhollyWhenAReferenceRangeUnresolvable(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "greet", "greet.go")

	// Fixed before mutating the file on disk: this occurrence's own line
	// (the declaration, "func Hello(name string) Greeting {") is never
	// touched below, so its position is unaffected either way.
	pos := identPosition(t, path, 1)

	original, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const callSiteLine = `	g := Hello("world")`
	if !strings.Contains(string(original), callSiteLine) {
		t.Fatalf("test fixture: %q not found in %s", callSiteLine, path)
	}
	// Far shorter than callSiteLine, so the facts index's recorded column
	// for this occurrence of "Hello" falls past this new line's end --
	// same line count, so every other occurrence's line number is
	// unaffected, and the file is never reopened through didOpen, so
	// dirtyRenameFiles' unsaved-edits guard never sees this file as dirty.
	mutated := strings.Replace(string(original), callSiteLine, "\tx", 1)
	if err := os.WriteFile(filepath.Clean(path), []byte(mutated), 0o600); err != nil {
		t.Fatalf("write mutated %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Clean(path), original, 0o600); err != nil {
			t.Fatalf("restore %s: %v", path, err)
		}
	})

	result, err := s.handleRename(context.Background(), mustMarshal(t, &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
		NewName: "Greet",
	}))
	if result != nil {
		t.Fatalf("handleRename(unresolvable reference range) result = %#v, want nil: never a WorkspaceEdit missing a reference silently", result)
	}
	if err == nil {
		t.Fatal("handleRename(unresolvable reference range) error = nil, want a loud error refusing the rename")
	}
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("handleRename(unresolvable reference range) error type = %T, want *rpc.Error", err)
	}
	if !strings.Contains(rpcErr.Message, "could not be resolved") {
		t.Errorf("handleRename(unresolvable reference range) error message = %q, want it to explain that a reference could not be resolved", rpcErr.Message)
	}
}
