// Package a imports its sibling root package b — see
// TestEngine_RootToRootImport_DoesNotSourceCheckThroughDepexport.
package a

import "example.com/rootimport/b"

// UseB calls b.Value.
func UseB() int {
	return b.Value()
}
