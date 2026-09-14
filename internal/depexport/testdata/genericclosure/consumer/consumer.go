// Package consumer references box.Box instantiated with payload.Data
// directly in an exported function SIGNATURE (not merely inside a function
// body, which declaration-only checking never type-checks), so resolving
// it requires importing both box and payload while checking consumer's own
// declarations.
package consumer

import (
	"example.com/genericclosure/box"
	"example.com/genericclosure/payload"
)

// F returns r's Msg field.
func F(r *box.Box[payload.Data]) *payload.Data {
	return r.Msg
}
