// Package broken has an intentional type error for testing error collection.
package broken

// Bad returns a string literal where an int is required.
func Bad() int {
	return "not an int"
}
