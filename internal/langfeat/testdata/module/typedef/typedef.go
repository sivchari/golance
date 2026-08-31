package typedef

import "example.com/langfeatmod/typedefdep"

// Local is declared in this package.
type Local struct {
	Name string
}

// UseLocal has a type declared in this same package.
var UseLocal Local

// UseRemote has a type declared in a different package.
var UseRemote typedefdep.Remote

// UsePointer has a pointer-to-named type, exercising namedTypeOf's unwrap.
var UsePointer *Local
