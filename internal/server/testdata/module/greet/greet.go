// Package greet provides a small, deterministic fixture for
// internal/server's wiring tests: one type, one function, and a caller so
// hover, definition, references, document symbols, and completion all have
// something real to resolve against.
package greet

// Greeting is a friendly message.
type Greeting struct {
	// Text is the message body.
	Text string
}

// Hello returns a Greeting for name.
func Hello(name string) Greeting {
	return Greeting{Text: "hello, " + name}
}

func useHello() string {
	g := Hello("world")
	return g.Text
}
