// Package depuse2 uses the same standard library symbol as depuse1
// (strings.TrimSpace), for TestReferences_DependencySymbol.
package depuse2

import "strings"

// CleanTwice trims s twice.
func CleanTwice(s string) string {
	return strings.TrimSpace(strings.TrimSpace(s))
}
