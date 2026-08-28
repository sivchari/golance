package index

import (
	"path/filepath"
	"strings"
)

// relPath returns path relative to root, for storage in the facts blob's
// file table and [store.UnitPointer].Files when Options.RelativePaths is
// set. It falls back to path unchanged if path does not lie under root
// (should not happen for a workspace package's own GoFiles, but keeps a
// write from failing outright); the reader side (internal/xref, and this
// package's own filesStatMatch) must be told the same RelativePaths value a
// database was written with to interpret its stored paths correctly.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
