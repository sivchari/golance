// Package dep is a leaf package testdata for internal/typecheck: no
// workspace dependencies, one stdlib import.
package dep

import "fmt"

// Greet returns a greeting for name.
func Greet(name string) string {
	return fmt.Sprintf("hello, %s", name)
}
