package inpkgtest

import "example.com/xrefmod/impl"

// useImplPerson exists only in this in-package "_test.go" file; it
// references impl.Person — a symbol declared in a different workspace
// package — from a _test.go position. graph.Package.GoFiles never lists
// this file at all (see internal/graph's loadMode), so resolving a
// position here depends on Resolver's directory fallback rather than an
// exact fileToPkg hit.
func useImplPerson() impl.Person {
	return impl.Person{Name: "t"}
}
