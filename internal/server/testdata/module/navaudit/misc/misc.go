// Package misc is the navigation-audit fixture's catch-all corner:
// shadowing, method values, a struct embedding chain, an anonymous struct,
// a closure, and a labeled loop.
package misc

// Shadow returns 1: the inner x shadows the outer one and never escapes its
// block.
func Shadow() int {
	x := 1
	{
		x := 2
		_ = x
	}
	return x
}

// Counter has a pointer-receiver method used as a method value below.
type Counter struct {
	n int
}

// Inc increments c's counter.
func (c *Counter) Inc() {
	c.n++
}

// UseMethodValue binds Counter.Inc as a method value and calls it.
func UseMethodValue() int {
	c := &Counter{}
	f := c.Inc
	f()
	f()
	return c.n
}

// A is the base of a three-level embedding chain.
type A struct{}

// M is promoted through B and C to any embedder.
func (A) M() string {
	return "A"
}

// B embeds A.
type B struct {
	A
}

// C embeds B, so C.M() resolves through two levels of promotion.
type C struct {
	B
}

func useChain() string {
	var c C
	return c.M()
}

// Anon is an anonymous struct value.
var Anon = struct {
	X int
	Y string
}{X: 1, Y: "y"}

// MakeAdder returns a closure over base.
func MakeAdder(base int) func(int) int {
	return func(n int) int {
		return base + n
	}
}

// Labeled uses a labeled continue to skip to the next outer iteration.
func Labeled() int {
	total := 0
Outer:
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if j == 1 {
				continue Outer
			}
			total += j
		}
		total += i
	}
	return total
}
