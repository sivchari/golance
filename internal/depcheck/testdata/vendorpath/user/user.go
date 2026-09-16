// Package user imports net/http, which transitively reaches "net" and
// "crypto/tls" — both of which, on a unix-like GOOS, import GOROOT-internal
// vendored golang.org/x/{net,crypto} packages by their literal (non
// "vendor/"-prefixed) source text (see
// GraphMetadataSource_ResolvesGorootVendoredStdlibImports_test.go).
package user

import "net/http"

// Get is a trivial reference to net/http, just enough to pull it (and its
// own transitive closure) into this fixture module's own import graph.
var Get = http.Get
