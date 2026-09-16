package xref

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// writeSplitIdentityFixture writes a module rooted in t.TempDir() shaped to
// trigger exportCache's coarse reset mid-confirmation: an interface method
// takes a shared dependency-package type (shared.Ctx) as a parameter, and
// numImpls concrete implementers -- each with a GENERIC receiver, so
// registerMethodSet leaves their Fingerprint at 0 and implementingTypes'
// fingerprint fast path can never confirm them, forcing every one through
// the decode-and-types.Implements fallback -- implement it the same way,
// each also referencing shared.Ctx. use.go adds one concrete-typed call
// site per implementer (never through the interface type), so References on
// the interface method's own SymbolID has no direct postings of its own:
// every result comes from correspondingMethodSymbols' confirmed-implementer
// union, the same confirmation implementingTypes runs for Implementation.
func writeSplitIdentityFixture(t *testing.T, numImpls int) (root, ifacePkg string) {
	t.Helper()
	root = t.TempDir()
	writeTestFile(t, root, "go.mod", "module example.com/xrefsplit\n\ngo 1.23\n")
	writeTestFile(t, root, "shared/shared.go", `package shared

type Ctx struct {
	A, B, C, D, E int
}
`)
	writeTestFile(t, root, "iface/iface.go", `package iface

import "example.com/xrefsplit/shared"

type Repository interface {
	Get(ctx shared.Ctx) error
}
`)
	var calls, imports string
	for i := 0; i < numImpls; i++ {
		name := fmt.Sprintf("impl%d", i)
		writeTestFile(t, root, filepath.Join(name, name+".go"), fmt.Sprintf(`package %s

import "example.com/xrefsplit/shared"

type Repo[T any] struct{}

func (r *Repo[T]) Get(ctx shared.Ctx) error { return nil }
`, name))
		imports += fmt.Sprintf("\t%s \"example.com/xrefsplit/%s\"\n", name, name)
		calls += fmt.Sprintf("func Call%d() error {\n\tvar r %s.Repo[int]\n\treturn r.Get(shared.Ctx{})\n}\n\n", i, name)
	}
	src := fmt.Sprintf("package use\n\nimport (\n\t\"example.com/xrefsplit/shared\"\n%s)\n\n%s", imports, calls)
	writeTestFile(t, root, "use/use.go", src)
	return root, "example.com/xrefsplit/iface"
}

// splitFixtureExportCap sits strictly between one package's own decoded
// export size (iface: 377 bytes; each implN: 437 bytes, probed empirically
// from this fixed fixture shape) and two packages' combined size, so
// resolving iface plus every implN candidate in sequence crosses it exactly
// once per extra candidate -- enough to force exportCache's coarse reset
// between candidates within a single confirmation pass, without a fixture
// large enough to approach exportCacheBytes' real-world default.
const splitFixtureExportCap = 800

func TestImplementation_ExportCacheResetMidQuery_DoesNotSplitConfirmation(t *testing.T) {
	root, ifacePkg := writeSplitIdentityFixture(t, 3)

	baseline, snap := newBenchResolver(t, root)
	ifaceFile := goFile(t, snap, ifacePkg, "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Repository")
	ctx := context.Background()

	want, err := baseline.Implementation(ctx, ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (uncapped baseline): %v", err)
	}
	if len(want) != 3 {
		t.Fatalf("Implementation (uncapped baseline) = %d results, want 3 (all three generic implementers, each fingerprint-unconfirmable and so decode-fallback-confirmed)", len(want))
	}

	r, _ := newBenchResolver(t, root, WithExportCacheBytes(splitFixtureExportCap))
	got, err := r.Implementation(ctx, ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (tiny export cache cap): %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Implementation under a %d-byte export cache cap = %d results, want %d (identical to the uncapped baseline -- a mid-confirmation exportCache reset must not split candidate/interface type identity)", splitFixtureExportCap, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	runsAfterFirst := r.confirmRunCount()
	got2, err := r.Implementation(ctx, ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (repeat, tiny cap): %v", err)
	}
	if runsAfterRepeat := r.confirmRunCount(); runsAfterRepeat != runsAfterFirst {
		t.Errorf("repeat query added %d confirmation runs, want 0 (served from memo)", runsAfterRepeat-runsAfterFirst)
	}
	if len(got2) != len(want) {
		t.Errorf("repeat query = %d results, want %d", len(got2), len(want))
	}
}

func TestReferences_ExportCacheResetMidQuery_DoesNotCollapseCorrespondingMethods(t *testing.T) {
	root, ifacePkg := writeSplitIdentityFixture(t, 3)

	baseline, snap := newBenchResolver(t, root)
	ifaceFile := goFile(t, snap, ifacePkg, "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Get")
	ctx := context.Background()

	want, err := baseline.References(ctx, ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References (uncapped baseline): %v", err)
	}
	if len(want) != 3 {
		t.Fatalf("References (uncapped baseline) = %d results, want 3 (one concrete-typed call site per confirmed implementer; the interface method's own SymbolID has no direct call site in this fixture)", len(want))
	}

	r, _ := newBenchResolver(t, root, WithExportCacheBytes(splitFixtureExportCap))
	got, err := r.References(ctx, ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References (tiny export cache cap): %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("References under a %d-byte export cache cap = %d results, want %d (identical to the uncapped baseline)", splitFixtureExportCap, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	runsAfterFirst := r.confirmRunCount()
	got2, err := r.References(ctx, ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References (repeat, tiny cap): %v", err)
	}
	if runsAfterRepeat := r.confirmRunCount(); runsAfterRepeat != runsAfterFirst {
		t.Errorf("repeat query added %d confirmation runs, want 0 (served from memo)", runsAfterRepeat-runsAfterFirst)
	}
	if len(got2) != len(want) {
		t.Errorf("repeat query = %d results, want %d", len(got2), len(want))
	}
}
