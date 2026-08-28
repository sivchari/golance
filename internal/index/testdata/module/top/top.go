// Package top depends on both leaf and mid.
package top

import (
	"example.com/idxmod/leaf"
	"example.com/idxmod/mid"
)

// Run exercises both leaf and mid.
func Run(name string) string {
	direct := leaf.Hello(name).Message
	shouted := mid.Shout(name)
	return direct + shouted
}
