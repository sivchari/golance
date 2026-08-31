package inlay

import "example.com/langfeatmod/typedefdep"

// Local is declared in this package, for exercising InlayHints' package
// qualifier: a same-package type renders unqualified.
type Local struct{ N int }

// qualifierCases exercises InlayHints' package qualifier for both a
// same-package type (Local, unqualified) and a type from another package
// (typedefdep.Remote, rendered by its short package name only — never the
// full module path).
func qualifierCases() {
	local := Local{}
	remote := typedefdep.Remote{}
	_, _ = local, remote
}
