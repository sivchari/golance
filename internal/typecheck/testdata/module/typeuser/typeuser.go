// Package typeuser imports typedep and returns its type from an exported
// function, embedding typedep's declaration into typeuser's own export
// data (see typedep's own doc).
package typeuser

import "example.com/tcmod/typedep"

// New returns a typedep.Greeting.
func New() typedep.Greeting {
	return typedep.Greeting("hi")
}
