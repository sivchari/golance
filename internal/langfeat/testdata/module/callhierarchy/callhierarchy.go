// Package callhierarchy is internal/langfeat's fixture for
// CallHierarchyFuncAt/FuncDeclaration/OutgoingCalls (callhierarchy_test.go):
// a method reached only through an interface, a call site repeated twice
// (fromRanges aggregation), a call from inside a function literal, and
// calls into both the standard library and a different workspace package,
// alongside a builtin call OutgoingCalls must filter out.
package callhierarchy

import (
	"fmt"

	"example.com/langfeatmod/callhierarchy/other"
)

// Adder can add.
type Adder interface {
	Add(a, b int) int
}

// Calc implements Adder.
type Calc struct{}

// Add returns the sum of a and b.
func (c Calc) Add(a, b int) int {
	return a + b
}

// Caller calls Add twice through the Adder interface -- the same callee
// from two call sites -- then Describe once.
func Caller(a Adder) int {
	sum := a.Add(1, 2)
	sum = a.Add(sum, 3)
	return sum + Describe(sum)
}

// Describe calls into the standard library, a different workspace package,
// and a builtin (len) that OutgoingCalls must filter out.
func Describe(n int) int {
	s := fmt.Sprintf("%d", n)
	return len(s) + other.Double(n)
}

// WithLiteral calls Describe from inside a function literal, attributing
// the call to WithLiteral itself.
func WithLiteral() int {
	f := func() int {
		return Describe(1)
	}
	return f()
}
