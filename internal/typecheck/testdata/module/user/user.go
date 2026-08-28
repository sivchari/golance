// Package user imports dep, exercising an ExportSource-resolved dependency.
package user

import "example.com/tcmod/dep"

// Message returns dep's greeting for "world".
func Message() string {
	return dep.Greet("world")
}
