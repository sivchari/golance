// Package a imports its sibling root package b, which itself imports root
// package c — a two-hop root-to-root import chain — see rootexport_test.go.
package a

import "example.com/rootexport/b"

// UseB returns b.Value's Field, forcing a's own type-check to resolve
// c.Item's Field through b's own export data.
func UseB() int {
	return b.Value().Field
}
