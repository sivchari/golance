// Package q depends on p in production.
package q

import "example.com/testcycle/p"

// Q returns p.P() + 1.
func Q() int { return p.P() + 1 }
