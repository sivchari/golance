// Package leaf has no workspace dependencies.
package leaf

// Greeting is a friendly greeting.
type Greeting struct {
	Message string
}

// String implements fmt.Stringer.
func (g Greeting) String() string {
	return g.Message
}

// Hello returns a Greeting for name.
func Hello(name string) Greeting {
	return Greeting{Message: "hello " + name}
}
