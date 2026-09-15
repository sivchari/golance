// Package b is imported by package a — see rootexport_test.go.
package b

import "example.com/rootexport/c"

// Value returns an Item built from c.
func Value() c.Item {
	return c.Item{Field: 1}
}
