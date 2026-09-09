package basepkg

import "testing"

func TestComputeInPackage(t *testing.T) {
	if Compute() != 7 {
		t.Fatal("Compute() != 7")
	}
}
