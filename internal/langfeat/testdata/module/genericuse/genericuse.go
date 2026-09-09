// Package genericuse consumes genericdep's generic types and functions
// through instantiation, exercising DependencyDefinition's resolution of
// fields, methods, type names, and functions reached through an
// instantiated generic dependency type -- see definition_test.go's
// TestDependencyDefinition_Generics.
package genericuse

import "example.com/langfeatmod/genericdep"

// Concrete is the type argument genericdep's generic types and functions
// are instantiated with below.
type Concrete struct {
	Name string
}

// String implements genericdep.Stringer.
func (c Concrete) String() string { return c.Name }

// UseBoxField exercises the Msg field access (the connect.Request[T].Msg
// shape this fixture reproduces) on an instantiated Box[Concrete].
func UseBoxField() *Concrete {
	b := genericdep.Box[Concrete]{}
	return b.Msg
}

// UseBoxValueMethod exercises a value-receiver method call on an
// instantiated Box[Concrete].
func UseBoxValueMethod() string {
	b := genericdep.Box[Concrete]{}
	return b.ValueDescribe()
}

// UseBoxPointerMethod exercises a pointer-receiver method call on an
// instantiated Box[Concrete].
func UseBoxPointerMethod() string {
	b := &genericdep.Box[Concrete]{}
	return b.PointerDescribe()
}

// UseWrapperField exercises a field promoted from an embedded instantiated
// generic struct (Wrapper embeds Box[int]).
func UseWrapperField() *int {
	var w genericdep.Wrapper
	return w.Msg
}

// UseWrapperMethod exercises a method promoted from an embedded
// instantiated generic struct.
func UseWrapperMethod() string {
	var w genericdep.Wrapper
	return w.ValueDescribe()
}

// UseIdentityExplicit exercises a generic function instantiated with an
// explicit type argument at the call site.
func UseIdentityExplicit() Concrete {
	return genericdep.Identity[Concrete](Concrete{Name: "explicit"})
}

// UseIdentityInferred exercises a generic function instantiated with an
// inferred type argument at the call site.
func UseIdentityInferred() Concrete {
	return genericdep.Identity(Concrete{Name: "inferred"})
}

// UseConstraint exercises a type parameter's own constraint method call:
// t.String() resolves through genericdep.Stringer, declared in the
// dependency package.
func UseConstraint[T genericdep.Stringer](t T) string {
	return t.String()
}

// TypeNameVar exercises "Go to Definition" on the generic type name itself,
// used directly as a variable's type (not through a field/method selector).
var TypeNameVar genericdep.Box[Concrete]

// AliasVar exercises "Go to Definition" through a type alias of a nested
// generic instantiation (Box[Inner[int]]).
var AliasVar genericdep.BoxOfInner
