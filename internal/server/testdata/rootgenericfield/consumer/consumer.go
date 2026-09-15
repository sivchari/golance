// Package consumer references wrapper.Box instantiated with payload.Data,
// mirroring connectrpc.com/connect's Request[T].Msg where T is a ROOT
// (workspace) type — see internal/server's
// root-type-argument-through-non-root-generic regression test.
package consumer

import (
	"example.com/rootgenericfield/payload"
	"example.com/rootgenericfield/wrapper"
)

// ExtractField returns r's Msg field.
func ExtractField(r *wrapper.Box[payload.Data]) *payload.Data {
	return r.Msg
}
