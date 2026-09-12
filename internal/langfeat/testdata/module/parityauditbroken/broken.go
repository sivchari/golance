// Package parityauditbroken is a fixture for verifying
// publishDiagnostics: a deliberate type error (assigning a string
// constant to an int variable), isolated in its own package so it never
// affects any other fixture's type-check.
package parityauditbroken

// BadAssign assigns a string constant to an int-typed variable.
func BadAssign() int {
	var n int = "not a number"

	return n
}
