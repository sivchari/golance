// Package testpkg is the navigation-audit fixture's test-package corner: an
// exported symbol referenced from both an in-package _test.go and an
// external _test package.
package testpkg

// Symbol is referenced from both testpkg_test.go (in-package) and
// testpkg_ext_test.go (external package testpkg_test).
func Symbol() int {
	return 42
}
