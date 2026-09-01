// Package inpkgtest exists solely to give an in-package "_test.go" file
// (see inpkgtest_test.go) that references a symbol in a different
// workspace package, isolated from the other fixture packages' own
// reference-count assertions (e.g. TestReferences_SpansDefiningAndReferencingPackages'
// exact Person count over impl/user/user2).
package inpkgtest
