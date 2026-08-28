// Package b depends on a.
package b

import "example.com/simple/a"

// B returns a.A() + 1.
func B() int { return a.A() + 1 }
