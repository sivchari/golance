// Package inpkgtest exists solely to give an in-package "_test.go" file
// (see inpkgtest_test.go) that references a symbol in a different
// workspace package, isolated from the other fixture packages' own
// reference-count assertions (e.g. TestReferences_SpansDefiningAndReferencingPackages'
// exact Person count over impl/user/user2).
package inpkgtest

// unexportedHelper exists only for TestWorkspaceSymbol_IncludesUnexported:
// an unexported top-level func, isolated here for the same reason as this
// package's own doc above, so workspace/symbol's unexported-name coverage
// can be pinned without affecting any other fixture package's assertions.
func unexportedHelper() int { return 0 }
