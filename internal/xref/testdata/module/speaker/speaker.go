// Package speaker declares Speaker, implemented by both a value-receiver
// and a pointer-receiver type in speakerimpl.
package speaker

// Speaker can speak.
type Speaker interface {
	Speak() string
}
