// Package codelens is a fixture for TestFindGenerateLens and
// TestAndBenchmarkLenses' non-test-file cases: an ordinary .go file with a
// single go:generate directive and no import "C".
package codelens

//go:generate stringer -type=Kind

// Kind is an arbitrary exported type the go:generate directive above
// pretends to generate a String method for.
type Kind int

// Helper is an ordinary top-level function: it must never produce a Test
// or Benchmark code lens even though nothing else in this file should.
func Helper() int { return 0 }
