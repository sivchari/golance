// Package consumer has no production import of dep at all — only its
// in-package test file (consumer_test.go) does.
package consumer

// C returns 1.
func C() int { return 1 }
