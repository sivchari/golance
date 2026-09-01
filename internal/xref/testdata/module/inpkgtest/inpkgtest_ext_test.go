package inpkgtest_test

// ExternalOnly exists only in the external "_test"-suffixed test package
// variant for inpkgtest — a file testFilesInPackage's package-clause filter
// excludes from inpkgtest's own facts (see internal/index/testfiles.go's
// doc), pinning that Resolver's directory fallback in resolveAt does not
// blindly trust a directory match: fileIndexOf's check against the unit's
// own facts file table must still reject a position here.
func ExternalOnly() int {
	return 1
}
