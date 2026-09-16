// Package bobpkg mirrors github.com/stephenafamo/bob's top-level Mod
// interface.
package bobpkg

// Mod mirrors bob.Mod[T].
type Mod[T any] interface {
	Apply(T) error
}
