package langfeat

import "go/types"

// qualifier returns a types.Qualifier equivalent to gopls's rendering of
// type and object strings: a type or object declared in pkg (the package
// being edited) is printed unqualified, and one from any other package is
// printed with just that package's short name (other.Name(), e.g. "bar"
// for github.com/foo/bar), never its full import path.
//
// This differs from types.RelativeTo, which qualifies a non-local type by
// other.Path() — its full import path — making hover, signature help,
// completion, and inlay hint output unreadable for anything outside the
// package being edited.
func qualifier(pkg *types.Package) types.Qualifier {
	return func(other *types.Package) string {
		if other == pkg {
			return ""
		}
		return other.Name()
	}
}
