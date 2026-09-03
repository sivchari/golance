// Package callh is internal/server's fixture for
// textDocument/prepareCallHierarchy, callHierarchy/incomingCalls, and
// callHierarchy/outgoingCalls (handlers_callhierarchy_test.go): direct
// calls, method calls reached only through an interface, a call repeated
// twice from the same caller (fromRanges aggregation), a call from inside a
// function literal, and calls into both a different workspace package and
// the standard library, alongside a builtin call that outgoing calls must
// filter out. callh_test.go adds a call from an in-package _test.go file;
// ../callhuser calls Add from a different package, reachable only through
// the workspace facts index.
package callh

import (
	"fmt"

	"example.com/servermod/callhdep"
)

// Greeter can greet.
type Greeter interface {
	Greet() string
}

// Robot implements Greeter.
type Robot struct{}

// Greet returns a greeting.
func (r Robot) Greet() string {
	return "beep boop"
}

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}

// Caller calls Add twice -- the same callee from two call sites -- then
// Describe once, then greets through the Greeter interface.
func Caller(g Greeter) string {
	sum := Add(1, 2)
	sum = Add(sum, 3)
	return Describe(sum) + g.Greet()
}

// Describe calls into the standard library, a different workspace package,
// and a builtin (len) that outgoing calls must filter out.
func Describe(n int) string {
	s := fmt.Sprintf("%d", n)
	d := callhdep.Double(n)
	return s + fmt.Sprint(d) + fmt.Sprint(len(s))
}

// WithLiteral calls Add from inside a function literal, attributing the
// call to WithLiteral itself.
func WithLiteral() int {
	f := func() int {
		return Add(4, 5)
	}
	return f()
}
