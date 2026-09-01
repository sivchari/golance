// z_other.go declares a different package clause than the rest of this
// directory: it must be excluded from the ad-hoc unit resolveFiles builds
// for "fixture" (Engine.canonicalPackageName picks the clause of the first
// candidate file in sorted order, a_fixture.go's "fixture", and filters
// the directory down to files matching it).
package other

// Baz must never end up in the "fixture" ad-hoc unit's scope.
func Baz() {}
