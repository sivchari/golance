// Package fixture lives under a directory named "testdata": go/packages
// (and the go tool generally) always excludes "testdata" directories from
// "./..." patterns, so graph.Load never reports this package. It exists to
// exercise Engine's ad-hoc fallback (GraphSource.PackageForFile's final
// case) against a real graph.Snapshot that genuinely does not know about
// it, rather than a synthetic SnapshotSource.
package fixture

// Foo is referenced from b_fixture.go, a sibling file in the same
// directory and package clause: Engine.resolveFiles must join it into the
// same ad-hoc unit.
func Foo() int {
	return 1
}
