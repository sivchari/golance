// Package user imports dep and instantiates its generic Box type, for
// reproducing and pinning Provider.Decl's resolution of a field or method
// reached through an instantiated generic dependency type (see
// depcheck_test.go's TestProvider_Decl_Generic* tests).
package user

import "example.com/depcheckmod/dep"

// Payload is the type argument dep.Box is instantiated with below.
type Payload struct {
	Name string
}

// UseBox exercises the Msg field and both receiver method kinds on an
// instantiated dep.Box[Payload].
func UseBox() string {
	b := dep.Box[Payload]{Msg: &Payload{Name: "x"}}
	_ = b.Msg
	_ = b.ValueDescribe()
	p := &b
	_ = p.PointerDescribe()
	return b.Msg.Name
}
