// Package other is callhierarchy's fixture for a call into a different
// workspace package (see callhierarchy.go's Describe).
package other

// Double returns n doubled.
func Double(n int) int {
	return n * 2
}
