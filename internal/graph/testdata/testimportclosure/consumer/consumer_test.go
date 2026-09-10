package consumer

import (
	"testing"

	"example.com/closuretest/dep"
)

// TestC exercises consumer's in-package test variant: it imports dep, a
// workspace package no production file in this package imports at all —
// the H11 fixture shape (see TestSnapshot_ClosureUnits_IncludesTestOnlyImporter).
func TestC(t *testing.T) {
	if C() != 1 || dep.V() != 1 {
		t.Fatal("unexpected result")
	}
}
