package fixture

import "example.com/checkmod/testdata/fixture/nonexistent"

// UseNonexistent references an import path the graph does not know
// about: the dependency importer cannot resolve it, so this is an
// ordinary type error in diagnostics rather than a panic or a hang (see
// design-adhoc-packages.md's Phase 3 detail 3).
func UseNonexistent() int {
	return nonexistent.Value
}
