// Package unimported is a fixture for handlers_completion_unimported_test.go:
// a bare package-name prefix referencing a workspace package (greet, not
// yet imported here) for the shape-1 subtest, and a qualified selector on
// an unimported standard library package (fmt) for the shape-2 subtest.
package unimported

func packagePrefixSite() {
	var _ = gre
}

func selectorSite() string {
	return fmt.Sp
}
