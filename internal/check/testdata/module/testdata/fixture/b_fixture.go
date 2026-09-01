package fixture

// Bar calls Foo, declared in a_fixture.go: same-directory, same-package
// resolution inside the ad-hoc unit.
func Bar() int {
	return Foo()
}
