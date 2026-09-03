// Package inpkgtest exists solely to give an in-package "_test.go" file
// (see inpkgtest_test.go) that references a symbol in a different
// workspace package: inpkgtest_test.go's own import of impl only ever
// shows up in graph.Package.TestImports, never Imports/ClosureUnits (see
// TestImports' own doc), so this is what pins that the reverse reference
// index (populated directly from facts extraction, not a closure walk —
// see internal/xref's locationsForAll/postingsFor) is not bounded by that
// same production-only import graph gap. It used to be, and
// TestReferences_SpansDefiningAndReferencingPackages' exact Person count
// over impl/user/user2 (now impl/user/user2/inpkgtest) documents both
// sides of that history.
package inpkgtest

// unexportedHelper exists only for TestWorkspaceSymbol_IncludesUnexported:
// an unexported top-level func, isolated here for the same reason as this
// package's own doc above, so workspace/symbol's unexported-name coverage
// can be pinned without affecting any other fixture package's assertions.
func unexportedHelper() int { return 0 }
