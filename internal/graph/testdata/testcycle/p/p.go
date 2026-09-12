// Package p is a leaf in production, but its in-package test file imports
// q, which (in production) imports p itself — legal only because go list
// treats p's test variant ("p [p.test]") as a distinct compilation unit
// from plain "p" (see Package.TestImports's own doc).
package p

// P returns 1.
func P() int { return 1 }
