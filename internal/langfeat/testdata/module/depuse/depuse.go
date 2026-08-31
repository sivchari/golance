// Package depuse imports the standard library, for DependencyDefinition's
// off-workspace fallback path (see internal/langfeat's definition_test.go).
package depuse

import (
	"fmt"
	"strings"
)

// UseStdlib calls into two different standard library packages.
func UseStdlib() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("hello"))
	return b.String()
}
