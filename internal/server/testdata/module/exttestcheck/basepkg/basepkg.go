// Package basepkg is a fixture for the external "_test" package
// navigation/rename regression tests (see ../../../../externaltest_test.go):
// an exported symbol referenced from both an in-package _test.go file and
// an external "_test"-suffixed test package.
package basepkg

// This is referenced from both basepkg_test.go (in-package) and
// basepkg_ext_test.go (external package basepkg_test). The doc comment
// deliberately does not start with the function's own name, so a rename
// here never also exercises gopls's separate "doc comment starts with the
// symbol's own name" convention-based rewrite -- unrelated to what this
// fixture tests.
func Compute() int {
	return 7
}
