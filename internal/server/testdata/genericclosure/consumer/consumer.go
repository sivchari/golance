// Package consumer references box.Box instantiated with payload.Data in an
// exported function, mirroring connectrpc.com/connect's Request[T].Msg —
// the shape internal/server's generic-wrapper hover/definition regression
// test exercises through the real, production check.Engine + depCacheHolder
// wiring (see ExtractField).
package consumer

import (
	"example.com/genericclosure/box"
	"example.com/genericclosure/payload"
)

// ExtractField returns r's Msg field.
func ExtractField(r *box.Box[payload.Data]) *payload.Data {
	return r.Msg
}
