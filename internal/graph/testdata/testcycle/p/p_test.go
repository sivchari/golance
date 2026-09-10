package p

import (
	"testing"

	"example.com/testcycle/q"
)

// TestP imports q, which (in production) imports p — the legal
// in-package-test-only back-edge this fixture exists to exercise.
func TestP(t *testing.T) {
	if P() != 1 || q.Q() != 2 {
		t.Fatal("unexpected result")
	}
}
