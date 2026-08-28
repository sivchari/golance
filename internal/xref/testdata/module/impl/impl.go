// Package impl declares Person, which implements iface.Greeter structurally
// (impl does not import iface).
package impl

// Person implements Greeter via Greet.
type Person struct {
	Name string
}

// Greet returns a greeting.
func (p Person) Greet() string {
	return "hello " + p.Name
}

// NewPerson constructs a Person.
func NewPerson(name string) Person {
	return Person{Name: name}
}
