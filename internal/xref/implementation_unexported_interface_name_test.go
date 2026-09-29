package xref

import (
	"context"
	"testing"
)

// TestImplementation_UnexportedInterfaceOwnNameListsImplementer closes one
// of the rare remaining holes the F1-F7 fixes left (F3/F8 in this fix's own
// audit): Implementation queried on an UNEXPORTED interface's own name
// (rather than one of its methods, which implementationOfMethod already
// handles via methodReceiverKey) still called resolveNamed directly at the
// top of Implementation, failing outright for the same structural reason
// every other fixed path did. Reuses the same facts-only implementer
// machinery (implementingTypesByKey/ownMethodEntries) implementationOfMethod
// already established.
func TestImplementation_UnexportedInterfaceOwnNameListsImplementer(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/unexportedifacename\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

// fetcher is unexported and never referenced by any exported signature: its
// own package's export data never carries it at all.
type fetcher interface {
	BatchGet() []int
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Repo struct{}

func (r Repo) BatchGet() []int { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/unexportedifacename/iface", "iface.go")
	implFile := goFile(t, snap, "example.com/unexportedifacename/impl", "impl.go")

	line, col := identOccurrence(t, ifaceFile, "fetcher")
	locs, err := r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(fetcher): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(fetcher) = %+v, want exactly 1 result (Repo)", locs)
	}
	wantLoc(t, locs, implFile, "Repo")
}
