// Package typeh is internal/server's fixture for
// textDocument/prepareTypeHierarchy, typeHierarchy/supertypes, and
// typeHierarchy/subtypes (handlers_typehierarchy_test.go): an interface I
// (one method), an interface J that satisfies I by having a superset of its
// method set, and a concrete type S implementing both -- the same shape
// gopls's own type hierarchy marker test poses its queries against.
// ../typehdep declares the structurally identical BI/BJ/BS, reachable from
// a query on these types only through the workspace facts index.
package typeh

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
