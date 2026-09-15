// Package b is imported by package a — see
// TestEngine_RootToRootImport_DoesNotSourceCheckThroughDepexport.
package b

// Value returns 1.
func Value() int {
	return 1
}
