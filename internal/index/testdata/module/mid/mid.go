// Package mid depends on leaf.
package mid

import (
	"strings"

	"example.com/idxmod/leaf"
)

// Shout returns an uppercase greeting for name.
func Shout(name string) string {
	g := leaf.Hello(name)
	return strings.ToUpper(g.Message)
}
