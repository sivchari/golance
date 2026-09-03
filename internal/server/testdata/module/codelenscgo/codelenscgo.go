// Package codelenscgo is a fixture for TestHandleCodeLens's regenerate_cgo
// case: an `import "C"` declaration, isolated in its own package so the
// unresolved "C" import cannot affect any other fixture package's
// type-check.
package codelenscgo

import "C"

// Noop exists so this package has at least one ordinary declaration beside
// the cgo import.
func Noop() {}
