package dep

import "testing"

// TestGreet exists only so this package has a test-variant import of
// "testing" — depcheck's own tests use it to verify Provider can check a
// test-only dependency the same as any other (Phase 1's graph loads test
// variants; see internal/graph's package doc).
func TestGreet(t *testing.T) {
	if got := Greet("world"); got != "hello, world" {
		t.Errorf("Greet(world) = %q, want %q", got, "hello, world")
	}
}
