package xref

import (
	"context"
	"go/types"
	"testing"
)

// TestImplementation_ReusesConfirmationAcrossRepeatQueries pins the memo
// fix for Implementation's own candidate-confirmation cost: a repeat query
// for the identical interface must not re-run implementingTypes/
// embeddingInterfaces' LookupMethod-plus-fingerprint/decode loop, the "1.4s
// per implementation call, unchanged on repeats" field-measured cost this
// memo exists to close (see Resolver.implementingTypesMemo's doc).
func TestImplementation_ReusesConfirmationAcrossRepeatQueries(t *testing.T) {
	root := generateBenchModule(t, 40)
	r, snap := newBenchResolver(t, root)
	targetFile := benchTargetFile(t, snap)
	line, col := benchIdentPos(t, targetFile, "Iface")
	ctx := context.Background()

	first, err := r.Implementation(ctx, targetFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (first query): %v", err)
	}
	if len(first) == 0 {
		t.Fatalf("Implementation (first query) returned no results")
	}
	runsAfterFirst := r.confirmRunCount()
	if runsAfterFirst == 0 {
		t.Errorf("confirmRunCount() after first query = 0, want > 0 (implementingTypes/embeddingInterfaces should have run their confirmation loop)")
	}

	second, err := r.Implementation(ctx, targetFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (repeat query): %v", err)
	}
	runsAfterRepeat := r.confirmRunCount()
	if runsAfterRepeat != runsAfterFirst {
		t.Errorf("repeat query added %d confirmation runs, want 0 (implementingTypes/embeddingInterfaces should have been served from their memo)", runsAfterRepeat-runsAfterFirst)
	}
	if len(second) != len(first) {
		t.Errorf("repeat query returned %d results, want %d (identical to the first query)", len(second), len(first))
	}
}

// TestReferences_InterfaceMethod_ReusesConfirmationAcrossRepeatQueries is
// TestImplementation_ReusesConfirmationAcrossRepeatQueries' counterpart for
// References on an interface method (correspondingMethodSymbols ->
// methodImplementationSymbols -> implementingTypes): the second call chain
// the field report's phase timer singled out (dominant_phase=resolve,
// ~670ms per call, unchanged on repeats).
func TestReferences_InterfaceMethod_ReusesConfirmationAcrossRepeatQueries(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refcorrmemo\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Talker interface {
	Say() string
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Concrete struct{}

func (c Concrete) Say() string { return "hi" }
`)
	writeTestFile(t, dir, "use/use.go", `package use

import (
	"example.com/refcorrmemo/iface"
	"example.com/refcorrmemo/impl"
)

func CallInterface(t iface.Talker) string {
	return t.Say()
}

func CallConcrete() string {
	return impl.Concrete{}.Say()
}
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/refcorrmemo/iface", "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Say")
	ctx := context.Background()

	first, err := r.References(ctx, ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References (first query): %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("References (first query) = %+v, want 2 (interface-typed call + concrete-typed call)", first)
	}
	runsAfterFirst := r.confirmRunCount()
	if runsAfterFirst == 0 {
		t.Errorf("confirmRunCount() after first query = 0, want > 0 (methodImplementationSymbols -> implementingTypes should have run its confirmation loop)")
	}

	second, err := r.References(ctx, ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References (repeat query): %v", err)
	}
	runsAfterRepeat := r.confirmRunCount()
	if runsAfterRepeat != runsAfterFirst {
		t.Errorf("repeat query added %d confirmation runs, want 0 (implementingTypes should have been served from its memo)", runsAfterRepeat-runsAfterFirst)
	}
	if len(second) != len(first) {
		t.Errorf("repeat query returned %d results, want %d (identical to the first query)", len(second), len(first))
	}
}

// TestResolver_Invalidate_ClearsConfirmMemo pins Invalidate's own memo
// clearing (see its doc for why a full clear rather than a surgical
// per-package drop): a query after Invalidate must re-run confirmation
// instead of silently reusing a candidate set computed before the
// invalidated packages' facts changed, and must still return correct
// results afterward.
func TestResolver_Invalidate_ClearsConfirmMemo(t *testing.T) {
	root := generateBenchModule(t, 12)
	r, snap := newBenchResolver(t, root)
	targetFile := benchTargetFile(t, snap)
	line, col := benchIdentPos(t, targetFile, "Iface")
	ctx := context.Background()

	first, err := r.Implementation(ctx, targetFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (before Invalidate): %v", err)
	}
	runsBeforeInvalidate := r.confirmRunCount()
	if runsBeforeInvalidate == 0 {
		t.Fatalf("confirmRunCount() before Invalidate = 0, want > 0")
	}

	r.Invalidate([]string{benchModuleName + "/target"})

	second, err := r.Implementation(ctx, targetFile, line, col)
	if err != nil {
		t.Fatalf("Implementation (after Invalidate): %v", err)
	}
	runsAfterInvalidate := r.confirmRunCount()
	if runsAfterInvalidate <= runsBeforeInvalidate {
		t.Errorf("Invalidate did not force a re-run: confirmRunCount() stayed at %d, want > %d", runsAfterInvalidate, runsBeforeInvalidate)
	}
	if len(second) != len(first) {
		t.Errorf("Implementation after Invalidate returned %d results, want %d (same as before -- Invalidate must not change correctness, only freshness)", len(second), len(first))
	}
}

// TestImplementingTypes_MemoEvictsAtCap is implementingTypesMemo's own
// regression test, WithConfirmMemoCapacity's whole reason for existing: a
// cap small enough to cross within a handful of distinct interfaces forces
// confirmMemo's LRU eviction, so a later repeat query for an
// already-evicted interface is NOT free -- proving the cap actually bounds
// the memo's growth instead of being dead configuration.
func TestImplementingTypes_MemoEvictsAtCap(t *testing.T) {
	root, pkgPaths := writeMultiIfaceModule(t, 5)
	const tinyCap = 2
	r, _ := newBenchResolver(t, root, WithConfirmMemoCapacity(tinyCap))
	ctx := context.Background()

	ifaceOf := func(pkgPath string) (*types.Named, *types.Interface) {
		t.Helper()
		named, err := r.resolveNamed(ctx, pkgPath, "Iface")
		if err != nil {
			t.Fatalf("resolveNamed(%s): %v", pkgPath, err)
		}
		iface, ok := named.Underlying().(*types.Interface)
		if !ok {
			t.Fatalf("%s.Iface underlying is not an interface", pkgPath)
		}
		return named, iface
	}

	for _, p := range pkgPaths {
		named, iface := ifaceOf(p)
		if _, err := r.implementingTypes(ctx, named, iface, []string{"M"}, newImplDiag([]string{"M"})); err != nil {
			t.Fatalf("implementingTypes(%s) (first pass): %v", p, err)
		}
	}
	runsAfterFirst := r.confirmRunCount()
	if runsAfterFirst != int64(len(pkgPaths)) {
		t.Fatalf("confirmRunCount() after first pass = %d, want %d (one run per distinct interface)", runsAfterFirst, len(pkgPaths))
	}
	if got := r.implementingTypesMemo.len(); got != tinyCap {
		t.Fatalf("implementingTypesMemo.len() after first pass = %d, want %d (bounded by WithConfirmMemoCapacity)", got, tinyCap)
	}

	// pkgPaths[0] was the first inserted and so the first evicted under a
	// tinyCap LRU once later, distinct interfaces pushed it out; re-querying
	// it must re-run confirmation instead of hitting a stale memo slot.
	named, iface := ifaceOf(pkgPaths[0])
	if _, err := r.implementingTypes(ctx, named, iface, []string{"M"}, newImplDiag([]string{"M"})); err != nil {
		t.Fatalf("implementingTypes(%s) (repeat, after eviction): %v", pkgPaths[0], err)
	}
	runsAfterRepeat := r.confirmRunCount()
	if runsAfterRepeat != runsAfterFirst+1 {
		t.Errorf("repeat query for an evicted interface added %d confirmation runs, want exactly 1 (the cap should have evicted it)", runsAfterRepeat-runsAfterFirst)
	}
}
