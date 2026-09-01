package b_test

import (
	"testing"

	"example.com/simple/b"
	"example.com/simple/c"
)

// TestB exercises b's external "_test" test variant: a distinct PkgPath
// ("example.com/simple/b_test") from b's own, materialized as its own graph
// node (see fromPackages's doc). It also imports c, which (in production)
// imports b itself — legal only because it is an EXTERNAL test package: its
// own PkgPath differs from b's, so this is not a real import cycle (an
// in-package test cannot do this at all; go list itself rejects it). This
// is exactly the case fromPackages must not turn into a topoOrder cycle by
// merging an external test package's imports into anything: b_test stays
// its own node, downstream of both b and c, and is never anyone's
// dependency in turn.
func TestB(t *testing.T) {
	if b.B() != 2 || c.C() != 3 {
		t.Fatal("unexpected result")
	}
}
