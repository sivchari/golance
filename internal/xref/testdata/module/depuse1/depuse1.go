// Package depuse1 uses a standard library symbol (strings.TrimSpace), for
// TestReferences_DependencySymbol.
package depuse1

import "strings"

// Clean trims s.
func Clean(s string) string {
	return strings.TrimSpace(s)
}
