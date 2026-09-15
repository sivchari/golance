// Package box provides a generic wrapper mirroring connectrpc.com/connect's
// Request[T]/Response[T] shape (a Msg *T field) — see
// internal/depexport's export-production-must-not-decode regression test.
package box

// Box wraps a value of type T.
type Box[T any] struct {
	Msg *T
}
