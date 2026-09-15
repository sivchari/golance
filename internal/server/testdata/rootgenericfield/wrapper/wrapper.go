// Package wrapper provides a generic wrapper mirroring connectrpc.com/
// connect's Request[T] shape (a Msg *T field), loaded as a NON-root
// (dependency-only) package — see internal/server's
// root-type-argument-through-non-root-generic regression test.
package wrapper

// Box wraps a value of type T.
type Box[T any] struct {
	Msg *T
}
