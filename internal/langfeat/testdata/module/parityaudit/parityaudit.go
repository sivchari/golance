// Package parityaudit is a fixture for verifying golance's signatureHelp,
// foldingRange, and publishDiagnostics answers against a real gopls
// oracle: generics, value/pointer receiver methods, variadics,
// multi-return/named results, embedded types, interfaces, a const block,
// struct literals, and a long function body with nested blocks and a
// multi-line comment.
package parityaudit

import (
	"errors"
	"fmt"
)

// Weight is a constraint satisfied by any integer or floating-point type.
type Weight interface {
	~int | ~float64
}

// Largest returns the largest value among vals.
func Largest[T Weight](vals ...T) T {
	best := vals[0]
	for _, v := range vals[1:] {
		if v > best {
			best = v
		}
	}
	return best
}

// Point is a 2D coordinate.
type Point struct {
	X, Y int
}

// Scale returns p scaled by factor, leaving p itself unchanged.
func (p Point) Scale(factor int) Point {
	return Point{X: p.X * factor, Y: p.Y * factor}
}

// Translate shifts p in place by (dx, dy).
func (p *Point) Translate(dx, dy int) {
	p.X += dx
	p.Y += dy
}

// Named can report a display name.
type Named interface {
	Name() string
}

// Shape embeds Point for its promoted Scale/Translate methods, and adds
// its own Label.
type Shape struct {
	Point
	Label string
}

// Name returns s's Label.
func (s Shape) Name() string { return s.Label }

// NewShape constructs a Shape at (x, y) with the given label.
func NewShape(x, y int, label string) Shape {
	return Shape{Point: Point{X: x, Y: y}, Label: label}
}

// Priority is a task's urgency.
type Priority int

// Priority levels, from lowest to highest.
const (
	PriorityLow Priority = iota
	PriorityMedium
	PriorityHigh
)

// errDivideByZero is Divide's error when b is zero.
var errDivideByZero = errors.New("parityaudit: divide by zero")

// Divide returns a divided by b as (quotient, remainder), or a non-nil err
// if b is zero.
func Divide(a, b int) (quotient, remainder int, err error) {
	if b == 0 {
		err = errDivideByZero
		return
	}
	quotient, remainder = a/b, a%b

	return
}

// BuildReport summarizes shapes by priority, in a single long function
// body with several nested blocks so it exercises deeply nested folding
// regions alongside this comment block, which itself spans several
// lines to give foldingRange something to fold besides code.
func BuildReport(shapes []Shape, priorities []Priority) string {
	counts := map[Priority]int{}
	names := map[Priority]string{}
	for i, s := range shapes {
		if i >= len(priorities) {
			break
		}
		p := priorities[i]
		switch {
		case p == PriorityHigh:
			counts[p]++
		case p == PriorityMedium:
			counts[p]++
		default:
			counts[p]++
		}
		names[p] = s.Name()
	}

	report := ""
	for p, n := range counts {
		if n == 0 {
			continue
		}
		report += fmt.Sprintf("priority %d (%s): %d shape(s)\n", Largest(int(p), 0), names[p], n)
	}

	return report
}

// useDivideAndTranslate calls Divide (multi-return, named results) and
// Translate (pointer receiver), giving signatureHelp comparisons fixed
// call-site positions to anchor on.
func useDivideAndTranslate() {
	q, r, err := Divide(10, 3)
	_ = q
	_ = r
	_ = err

	p := &Point{X: 1, Y: 2}
	p.Translate(3, 4)
}
