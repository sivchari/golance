// Package iface is the navigation-audit fixture's interface corner: an
// embedded interface and methods promoted through struct embedding.
package iface

// Speaker can speak.
type Speaker interface {
	Speak() string
}

// Greeter embeds Speaker and adds Name.
type Greeter interface {
	Speaker
	Name() string
}

// Base implements Speaker.
type Base struct{}

// Speak implements Speaker.
func (Base) Speak() string {
	return "..."
}

// Named embeds Base (promoting Speak) and adds Name, so Named implements
// Greeter without declaring Speak itself.
type Named struct {
	Base
	N string
}

// Name implements the rest of Greeter.
func (n Named) Name() string {
	return n.N
}

func useGreeter(g Greeter) string {
	return g.Name() + ": " + g.Speak()
}

func callWithNamed() string {
	return useGreeter(Named{N: "gopher"})
}
