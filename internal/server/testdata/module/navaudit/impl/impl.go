// Package impl is the navigation-audit fixture's cross-package
// implementation corner: value vs pointer receiver satisfaction of an
// interface declared in ../iface.
package impl

import "example.com/servermod/navaudit/iface"

// ValueSpeaker satisfies iface.Speaker through a value receiver, so both
// ValueSpeaker and *ValueSpeaker implement it.
type ValueSpeaker struct{}

// Speak implements iface.Speaker.
func (ValueSpeaker) Speak() string {
	return "value"
}

// PtrSpeaker satisfies iface.Speaker only through *PtrSpeaker, since Speak
// has a pointer receiver.
type PtrSpeaker struct{}

// Speak implements iface.Speaker on *PtrSpeaker only.
func (*PtrSpeaker) Speak() string {
	return "pointer"
}

// FullGreeter implements iface.Greeter directly (no embedding).
type FullGreeter struct{}

// Speak implements part of iface.Greeter.
func (FullGreeter) Speak() string {
	return "hi"
}

// Name implements the rest of iface.Greeter.
func (FullGreeter) Name() string {
	return "full"
}

var (
	_ iface.Speaker = ValueSpeaker{}
	_ iface.Speaker = (*PtrSpeaker)(nil)
	_ iface.Greeter = FullGreeter{}
)

func useValueSpeaker() iface.Speaker {
	return ValueSpeaker{}
}
