package exttest

// Exported is visible to any importer, including this package's own
// external "_test" test unit.
func Exported() int {
	return 1
}

// unexported is visible only within this package's own checked unit — not
// to the external "_test" test package, a separate unit with its own
// *types.Package identity (see internal/check's unitKey).
func unexported() int {
	return 2
}
