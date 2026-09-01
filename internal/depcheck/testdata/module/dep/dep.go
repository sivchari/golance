// Package dep is a tiny fixture package depcheck's own tests check as if it
// were a non-workspace dependency (Provider does not itself distinguish
// root from non-root packages — that distinction belongs to whichever
// caller built its MetadataSource).
package dep

import (
	"fmt"
	"strings"
)

// Greet returns a greeting for name, built with strings.Builder.
func Greet(name string) string {
	var b strings.Builder
	b.WriteString("hello, ")
	b.WriteString(name)
	return fmt.Sprint(b.String())
}

// secret is an unexported package-level value, for exercising Decl's
// scope-lookup fallback (an unexported object objectpath cannot encode a
// path for).
var secret = "shh"

// secretLen returns len(secret), the only use of secret — keeps it live so
// a lint pass does not flag it as unused.
func secretLen() int { return len(secret) }
