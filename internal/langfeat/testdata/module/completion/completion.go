package completion

import "strings"

// Point is a simple 2D point.
type Point struct {
	X int
	Y int
}

// Add returns the sum of p and other.
func (p Point) Add(other Point) Point {
	return Point{X: p.X + other.X, Y: p.Y + other.Y}
}

// Scale multiplies p in place by factor.
func (p *Point) Scale(factor int) {
	p.X *= factor
	p.Y *= factor
}

func selectorSite(p Point) int {
	return p.X
}

func prefixSite(p Point) int {
	return p.Sc
}

func packageMemberSite(s string) string {
	return strings.ToUpper(s)
}

func lexicalSite() int {
	origin := Point{}
	total := origin.X
	return tot
}
