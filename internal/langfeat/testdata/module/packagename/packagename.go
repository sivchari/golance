// Package packagename imports the standard library both plainly and with an
// explicit alias, for PackageNameDefinition/PackageNameReferences' tests
// (internal/langfeat's definition_test.go).
package packagename

import (
	"fmt"
	str "strings"
)

// UsePlain uses the plain "fmt" package-qualifier identifier twice.
func UsePlain() string {
	fmt.Println("one")
	return fmt.Sprintf("two")
}

// UseAliased uses the aliased "str" package-qualifier identifier.
func UseAliased() string {
	return str.ToUpper("three")
}
