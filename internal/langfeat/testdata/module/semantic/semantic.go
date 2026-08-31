// Package semantic exercises every token kind and modifier
// langfeat.SemanticTokens classifies, for semantic_test.go.
package semantic

import "fmt"

// Shape is implemented by every shape in this package.
type Shape interface {
	Area() float64
}

// Rect is a rectangle.
type Rect struct {
	Width, Height float64
}

// Area returns r's area.
func (r Rect) Area() float64 {
	return r.Width * r.Height
}

// MaxShapes is the maximum number of shapes tracked.
const MaxShapes = 16

// count is a package-level variable.
var count int

// First returns xs's first element, using a generic type parameter.
func First[T any](xs []T) T {
	return xs[0]
}

// Describe formats r for display.
//
// Deprecated: use r.Area directly.
func Describe(r Rect) string {
	msg := fmt.Sprintf("rect %gx%g", r.Width, r.Height)
	return msg
}

func lengthOf(s string) int {
	return len(s)
}
