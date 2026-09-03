package callh

import "testing"

// TestAddFromTest calls Add from an in-package _test.go file, for
// callHierarchy/incomingCalls' "calls from _test.go files" case.
func TestAddFromTest(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("Add(1, 1) != 2")
	}
}
