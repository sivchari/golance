// Package c depends on both a and b.
package c

import (
	"example.com/simple/a"
	"example.com/simple/b"
)

// C returns a.A() + b.B().
func C() int { return a.A() + b.B() }
