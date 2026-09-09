// Package genericdep provides generic types and functions for exercising
// DependencyDefinition's resolution of a field, method, or type name reached
// through an INSTANTIATED generic dependency type (see definition_test.go's
// TestDependencyDefinition_Generics).
package genericdep

// Box wraps a value of type T, mirroring connectrpc.com/connect's
// Request[T]/Response[T] shape (a Msg *T field) that motivated this
// fixture.
type Box[T any] struct {
	Msg *T
}

// ValueDescribe is a value-receiver method on the generic Box type.
func (b Box[T]) ValueDescribe() string { return "box" }

// PointerDescribe is a pointer-receiver method on the generic Box type.
func (b *Box[T]) PointerDescribe() string { return "box" }

// Wrapper embeds an instantiated Box, for exercising a field and a method
// promoted from an embedded instantiated generic struct.
type Wrapper struct {
	Box[int]
}

// Inner is a second generic type, for exercising nested instantiation
// (Box[Inner[T]]) via BoxOfInner below.
type Inner[T any] struct {
	Val T
}

// BoxOfInner is a generic type alias of a nested instantiation: Box
// instantiated with Inner[int] as its own type argument.
type BoxOfInner = Box[Inner[int]]

// Identity returns v unchanged: a plain generic function, instantiated at
// its call site rather than as part of a named type.
func Identity[T any](v T) T { return v }

// Stringer is a constraint a generic function's type parameter can be bound
// to, for exercising a type parameter's own constraint method call: t.String()
// resolves to Stringer's own declared method, never an instantiation
// artifact, since a type parameter's method set comes straight from its
// constraint interface.
type Stringer interface {
	String() string
}
