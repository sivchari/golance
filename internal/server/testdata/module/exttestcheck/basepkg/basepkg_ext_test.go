package basepkg_test

import (
	"testing"

	"example.com/servermod/exttestcheck/basepkg"
)

func TestComputeExternal(t *testing.T) {
	if basepkg.Compute() != 7 {
		t.Fatal("basepkg.Compute() != 7")
	}
}
