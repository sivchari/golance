package withtests_test

import "example.com/checkmod/withtests"

// UsesBaseExported calls Value, the base package's only exported
// declaration, resolved through the external test unit's ordinary
// dependency importer (see Engine.runRecheck's doc) exactly like any other
// cross-package import.
func UsesBaseExported() int {
	return withtests.Value() + ExternalOnly()
}

// ExternalOnly exists only in this external "_test" package variant, and
// must not join the base package's checked unit — they are separate units,
// see unitKey (TestEngine_Get_ExternalTestPackageFileExcluded pins this).
func ExternalOnly() int {
	return 2
}
