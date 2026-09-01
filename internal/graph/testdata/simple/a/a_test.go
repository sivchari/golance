package a

import "testing"

// TestA exercises a's in-package test variant: it imports "testing", a
// stdlib package no production file in this module imports at all (see
// TestLoad_TestOnlyImports).
func TestA(t *testing.T) {
	if A() != 1 {
		t.Fatal("unexpected A()")
	}
}
