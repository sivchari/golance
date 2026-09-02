// Package builtinuse is a fixture for handlers_xref_test.go's
// TestHandleDefinition_Builtin: a universe (predeclared) identifier,
// referenced from a workspace file, for "go to definition" to resolve into
// the toolchain's own builtin.go rather than nothing at all.
package builtinuse

func Count(v []int) int {
	return len(v)
}
