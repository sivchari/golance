package hover

import "strings"

// NewBuilder returns a fresh string-building value from the standard
// library, for TestHover_CrossPackage's cross-package hover query.
func NewBuilder() strings.Builder {
	return strings.Builder{}
}

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}

// Greeting is a friendly message.
type Greeting struct {
	// Text is the message body.
	Text string
}

// DefaultGreeting is the greeting used when none is supplied.
var DefaultGreeting = Greeting{Text: "hello"}
