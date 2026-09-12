package auditfeat_test

import (
	"testing"

	"example.com/langfeatmod/auditfeat"
)

// TestSum exercises Sum from an external test package, and doubles as a
// fixture for codelens/documentSymbol parity checks on a _test.go file.
func TestSum(t *testing.T) {
	if got := auditfeat.Sum(1, 2, 3); got != 6 {
		t.Errorf("Sum(1,2,3) = %d, want 6", got)
	}
}

// BenchmarkSum is a fixture for the file-level "run benchmarks" code lens.
func BenchmarkSum(b *testing.B) {
	for range b.N {
		auditfeat.Sum(1, 2, 3)
	}
}
