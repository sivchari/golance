// Package user references impl.Person and impl.NewPerson.
package user

import "example.com/xrefmod/impl"

// Declare references impl.Person by name twice: once as the return type,
// once as the composite literal type.
func Declare() impl.Person {
	return impl.Person{Name: "a"}
}

// Use calls Person's Greet method.
func Use() string {
	p := impl.NewPerson("b")
	return p.Greet()
}
