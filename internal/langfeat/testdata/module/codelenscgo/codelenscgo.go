// Package codelenscgo is a fixture for TestFindRegenerateCgoLens: an
// `import "C"` declaration, isolated in its own package so that the
// unresolved "C" import (this fixture is never actually cgo-preprocessed;
// only its AST shape is under test) cannot affect any other fixture
// package's type-check.
package codelenscgo

import "C"

// Noop exists so this package has at least one ordinary declaration beside
// the cgo import.
func Noop() {}
