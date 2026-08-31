// Package greeteruse references iface.Greeter as a type only, giving
// Implementation a use-site cursor position on the interface name that is
// distinct from Greeter's own declaration.
package greeteruse

import "example.com/xrefmod/iface"

// G is typed as iface.Greeter without holding a concrete value.
var G iface.Greeter
