package codelens

import "testing"

// TestAdd is a genuine top-level test: TestHandleCodeLens expects a run-test
// lens for it (once the "test" code lens source is enabled).
func TestAdd(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("math is broken")
	}
}

// BenchmarkAdd is a genuine top-level benchmark: TestHandleCodeLens expects
// both a run-benchmark lens for it and a "run file benchmarks" lens for the
// file.
func BenchmarkAdd(b *testing.B) {
	for range b.N {
	}
}

// FuzzAdd must never produce a lens: gopls's own code lens source has no
// fuzz-target regexp at all (see langfeat.TestAndBenchmarkLenses' doc).
func FuzzAdd(f *testing.F) {
	f.Fuzz(func(t *testing.T, x int) {})
}
