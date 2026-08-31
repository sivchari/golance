package rename

import "fmt"

// Greet uses fmt (a package name reference) and a local variable msg.
func Greet(name string) string {
	msg := fmt.Sprintf("hello, %s", name)
	return msg
}
