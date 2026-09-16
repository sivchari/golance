// Package modspkg mirrors github.com/stephenafamo/bob/mods: a generic
// pointer-receiver type plus a helper that returns it instantiated against
// example.com/genericsplit/shared.C, baking shared.C's *types.Named
// identity into modspkg's own declarations at whatever generation of
// "shared" was current when modspkg itself was checked.
package modspkg

import "example.com/genericsplit/shared"

// Set mirrors bob's mods.set[T].
type Set[T any] struct{}

// Apply mirrors mods.set[T].Apply: pointer receiver, satisfying bobpkg.Mod[T].
func (s *Set[T]) Apply(T) error { return nil }

// MakeDefault mirrors a helper returning *Set[shared.C].
func MakeDefault() *Set[shared.C] { return &Set[shared.C]{} }
