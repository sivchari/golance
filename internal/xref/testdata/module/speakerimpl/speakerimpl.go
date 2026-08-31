// Package speakerimpl declares two implementers of speaker.Speaker: one via
// a value receiver, one via a pointer receiver.
package speakerimpl

// ValSpeaker implements Speaker via a value receiver.
type ValSpeaker struct{}

// Speak returns a greeting.
func (v ValSpeaker) Speak() string {
	return "hello from ValSpeaker"
}

// PtrSpeaker implements Speaker via a pointer receiver.
type PtrSpeaker struct{}

// Speak returns a greeting.
func (p *PtrSpeaker) Speak() string {
	return "hello from PtrSpeaker"
}
