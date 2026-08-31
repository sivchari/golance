package folding

import (
	"fmt"
	"strings"
)

// Box is a foldable struct body.
type Box struct {
	Width  int
	Height int
}

// Nested exercises deeply nested block folding: func body -> if -> for.
func Nested(items []string) int {
	total := 0
	if len(items) > 0 {
		for _, item := range items {
			total += len(item)
		}
	}
	fmt.Println(strings.Join(items, ","))
	return total
}
