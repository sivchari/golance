// Package reexport is the navigation-audit fixture's import-shape corner: a
// dot import, an aliased import, and re-exported symbols from another
// workspace package.
package reexport

import (
	navg "example.com/servermod/navaudit/generics"
	. "example.com/servermod/navaudit/iface"
)

// ReBox re-exports an instantiated Box from ../generics through an aliased
// import.
type ReBox = navg.Box[int]

var ReVal = navg.IntBox{Value: 1}

// dotSpeaker uses Speaker, brought into scope by the dot import above.
func dotSpeaker(s Speaker) string {
	return s.Speak()
}

func dotGreeter(g Greeter) string {
	return g.Name()
}

func useReBox() int {
	b := ReBox{Value: 3}
	return b.Get()
}
