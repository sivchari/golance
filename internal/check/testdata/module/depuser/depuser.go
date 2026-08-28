package depuser

import "fmt"

// Greet returns a greeting for name, exercising a real stdlib dependency
// (fmt) so tests can observe whether its export data gets decoded once or
// repeatedly across rechecks.
func Greet(name string) string {
	return fmt.Sprintf("hello, %s", name)
}
