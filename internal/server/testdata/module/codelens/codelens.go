// Package codelens is a fixture for TestHandleCodeLens: an ordinary .go
// file with a single go:generate directive.
package codelens

//go:generate stringer -type=Kind

// Kind is an arbitrary exported type the go:generate directive above
// pretends to generate a String method for.
type Kind int
