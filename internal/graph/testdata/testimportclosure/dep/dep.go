// Package dep is a leaf package imported only by consumer's in-package test
// file, never by any production file in this module.
package dep

// V returns 1.
func V() int { return 1 }
