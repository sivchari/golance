// Package pkgcount is shared's only import: TestEngine_Get_RootImportCache
// DoesNotThrashUnderDefaultMaxLRU counts calls into it to detect how many
// times shared was actually rechecked (as opposed to served from Engine's
// cache), since shared's own already-cached *types.Package is returned
// without re-resolving pkgcount at all.
package pkgcount

// Value returns a constant, counted by the test's Importer wrapper.
func Value() int { return 0 }
