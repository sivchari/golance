// Package typehierarchy is internal/langfeat's fixture for
// TypeHierarchyPrepare (typehierarchy_test.go): an interface I (one
// method), an interface J that satisfies I by having a superset of its
// method set, and a concrete type S implementing both -- the same shape
// gopls's own type hierarchy marker test poses its queries against -- plus
// a reference to a type declared in a different workspace package
// (other.Remote) for the cross-package prepare branch.
package typehierarchy

import "example.com/langfeatmod/typehierarchy/other"

// I declares F.
type I interface {
	F()
}

// J declares F and G.
type J interface {
	F()
	G()
}

// S implements both I and J.
type S int

func (S) F() {}
func (S) G() {}

// Var references a cross-package type, for the cross-package prepare test.
var Var other.Remote

// notAType is a plain func, for the "not a type name" test.
func notAType() {}
