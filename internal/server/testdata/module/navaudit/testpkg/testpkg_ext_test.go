package testpkg_test

import (
	"testing"

	"example.com/servermod/navaudit/testpkg"
)

func TestSymbolExternal(t *testing.T) {
	if testpkg.Symbol() != 42 {
		t.Fatal("testpkg.Symbol() != 42")
	}
}
