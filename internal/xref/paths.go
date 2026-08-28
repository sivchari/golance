package xref

import (
	"path/filepath"
	"strings"
)

// relPath returns path relative to root, mirroring internal/index's own
// relPath: the transform applied to an incoming absolute file path before
// comparing it against a root-relative facts blob's file table (see
// Resolver.relative).
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// absPath reverses relPath: it joins stored back onto root when relative is
// set, so every exported Location.File stays absolute regardless of how the
// underlying facts database stores it (see the package doc's coordinate
// system note). stored already absolute (a database written without
// Options.RelativePaths) is returned unchanged.
func absPath(root, stored string, relative bool) string {
	if !relative || filepath.IsAbs(stored) {
		return stored
	}
	return filepath.Join(root, stored)
}
