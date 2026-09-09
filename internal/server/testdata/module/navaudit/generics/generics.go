// Package generics is the navigation-audit fixture's generics corner: an
// instantiated struct field, value and pointer receiver methods on a
// generic type, an embedded generic struct, a generic function called both
// with explicit and inferred type arguments, a constraint interface with a
// method, and a nested instantiation.
package generics

// Box holds a single value of type T.
type Box[T any] struct {
	Value T
}

// Get returns b's value (value receiver).
func (b Box[T]) Get() T {
	return b.Value
}

// Set replaces b's value (pointer receiver).
func (b *Box[T]) Set(v T) {
	b.Value = v
}

// IntBox is a type alias of an instantiated Box.
type IntBox = Box[int]

// Container embeds a generic Box.
type Container[T any] struct {
	Box[T]
	Label string
}

// MapSlice applies f to every element of s.
func MapSlice[T, U any](s []T, f func(T) U) []U {
	out := make([]U, len(s))
	for i, v := range s {
		out[i] = f(v)
	}
	return out
}

func useMapSliceExplicit() []string {
	return MapSlice[int, string](nil, nil)
}

func useMapSliceInferred() []int {
	return MapSlice([]int{1, 2, 3}, func(n int) int { return n * 2 })
}

// Adder is a constraint interface with a method.
type Adder[T any] interface {
	Add(T) T
}

// Money is a concrete type satisfying Adder[Money].
type Money int

// Add implements Adder[Money].
func (m Money) Add(other Money) Money {
	return m + other
}

// SumAdders sums items using their own Add method.
func SumAdders[T Adder[T]](items []T) T {
	var sum T
	for _, it := range items {
		sum = sum.Add(it)
	}
	return sum
}

func useSumAdders() Money {
	return SumAdders([]Money{1, 2, 3})
}

// NestedBox is a nested instantiation: a Box of a Box.
type NestedBox = Box[Box[int]]

var nested = Box[Box[int]]{Value: Box[int]{Value: 42}}

func useNested() int {
	return nested.Value.Get()
}

func useContainer() int {
	c := Container[int]{Box: Box[int]{Value: 7}, Label: "seven"}
	return c.Get()
}

func useBoxSetPointer() int {
	b := Box[int]{Value: 1}
	b.Set(2)
	return b.Value
}
