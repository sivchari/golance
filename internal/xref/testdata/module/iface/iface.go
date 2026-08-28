// Package iface declares Greeter, implemented by impl.Person in a
// different, non-importing package.
package iface

// Greeter can greet.
type Greeter interface {
	Greet() string
}
