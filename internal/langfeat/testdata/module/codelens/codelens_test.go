package codelens

import "testing"

// TestHelper is a fixture for TestAndBenchmarkLenses' signature-check case:
// its name matches the Test regexp, but its parameter is not *testing.T,
// so it must not produce a lens.
func TestHelper(x int) {}

// TestTooManyParams is a fixture for TestAndBenchmarkLenses' signature-check
// case: its name matches the Test regexp, and its first parameter is
// *testing.T, but it takes a second parameter too, so it must not produce a
// lens.
func TestTooManyParams(t *testing.T, want int) {}

// Testable is a fixture for TestAndBenchmarkLenses' name-regexp case: it
// starts with "Test" but is followed by a lowercase letter, so the regexp
// itself must reject it before any signature check runs.
func Testable(t *testing.T) {}

// TestAdd is a genuine top-level test: it must produce a run-test lens, and
// its subtest ("subtest") registered via t.Run must NOT — gopls's own
// runTestCodeLens only scans top-level *ast.FuncDecls, never t.Run calls.
func TestAdd(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"one", 1},
		{"two", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.want == 0 {
				t.Fatal("unexpected zero")
			}
		})
	}
}

// BenchmarkAdd is a genuine top-level benchmark: it must produce a
// run-benchmark lens, plus a "run file benchmarks" lens somewhere in the
// file (gopls emits exactly one, anchored at the package clause, whenever a
// file has at least one Benchmark function).
func BenchmarkAdd(b *testing.B) {
	for range b.N {
	}
}

// FuzzAdd is a fixture for TestAndBenchmarkLenses' recognition-rule case:
// gopls's own code lens source has no fuzz-target regexp at all, so this
// must never produce a lens even though `go test -list` would enumerate it
// as a test.
func FuzzAdd(f *testing.F) {
	f.Fuzz(func(t *testing.T, x int) {})
}

// ExampleAdd is a fixture for the same recognition-rule case as FuzzAdd:
// gopls's own code lens source has no Example regexp either.
func ExampleAdd() {}
