package testpkg

import "testing"

func TestSymbolInPackage(t *testing.T) {
	if Symbol() != 42 {
		t.Fatal("Symbol() != 42")
	}
}
