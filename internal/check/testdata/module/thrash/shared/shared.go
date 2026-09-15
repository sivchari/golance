// Package shared is imported by both leafa (indirectly) and top (directly,
// after several other packages) — see TestEngine_Get_RootImportCache
// DoesNotThrashUnderDefaultMaxLRU. Its own import of pkgcount is the test's
// counting hook.
package shared

import "example.com/checkmod/thrash/pkgcount"

// Value returns pkgcount's own value.
func Value() int { return pkgcount.Value() }
