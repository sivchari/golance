package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// TestSetWorkspace_ReusesEngineWhenDepsUnchanged covers setWorkspace's reuse
// path end to end: a reload of the same root whose non-workspace
// (stdlib/module-cache) package set is unchanged must reuse ws.engine and
// ws.graphSrc by pointer, not rebuild them (see setWorkspace's own doc for
// why that reuse is sound), while still resolving against the NEW snapshot
// afterward — graphSrc.Retarget swaps its index in place rather than the
// pointer itself.
func TestSetWorkspace_ReusesEngineWhenDepsUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	populateTempModule(t, root)

	snap1, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (first): %v", err)
	}
	s := newWorkspaceOnlyServerAt(t, root, snap1)
	ws1 := s.workspace()

	// A new file landing in greet's already-known directory is the only way
	// needsGraphReload triggers a reload without a go.mod/go.sum change
	// (see needsGraphReload's doc); the dependency set itself (none, here)
	// stays byte-identical, so this must land on setWorkspace's reuse path.
	newFile := writeTempFile(t, filepath.Join(root, "greet"), "greet_extra.go", "package greet\n\n// Extra is an additional exported function.\nfunc Extra() string { return \"extra\" }\n")

	snap2, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (second): %v", err)
	}
	if snap1 == snap2 {
		t.Fatal("graph.Load returned the same *graph.Snapshot twice; test setup is wrong")
	}
	s.setWorkspace(root, snap2)
	ws2 := s.workspace()

	if ws2.engine != ws1.engine {
		t.Error("setWorkspace rebuilt engine even though the dependency set was unchanged")
	}
	if ws2.graphSrc != ws1.graphSrc {
		t.Error("setWorkspace rebuilt graphSrc even though the dependency set was unchanged")
	}

	// The reused graphSrc must resolve against snap2, not the discarded
	// snap1: greet_extra.go did not exist when snap1/ws1 were built, so
	// only a Retarget (not a stale index) lets Get resolve it.
	if _, ok := ws2.fileToPkg[newFile]; !ok {
		t.Fatal("ws2.fileToPkg does not know about greet_extra.go; setWorkspace's own index was not rebuilt over snap2")
	}
	if _, err := ws2.engine.Get(context.Background(), newFile); err != nil {
		t.Errorf("engine.Get(%s) after reuse = %v, want nil (graphSrc.Retarget should have picked up the new file)", newFile, err)
	}
}

// TestSetWorkspace_RebuildsEngineWhenDepsChanged covers the correctness
// half of the same reuse path: once the non-workspace dependency set
// itself differs — here, a newly reachable standard-library import — reuse
// must not fire and a fresh engine/graphSrc must be installed. Mirrors
// TestEnsureDepProvider_RebuildsWhenDepsChanged's approach (a new package
// becoming reachable) at the setWorkspace level.
func TestSetWorkspace_RebuildsEngineWhenDepsChanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	populateTempModule(t, root)

	snap1, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (first): %v", err)
	}
	s := newWorkspaceOnlyServerAt(t, root, snap1)
	ws1 := s.workspace()

	greetGo := filepath.Join(root, "greet", "greet.go")
	writeTempFile(t, filepath.Join(root, "greet"), "greet.go", "package greet\n\nimport \"strings\"\n\n// Hello returns a greeting.\nfunc Hello() string { return strings.ToUpper(\"hi\") }\n")

	snap2, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (second): %v", err)
	}
	if _, ok := snap1.Packages["strings"]; ok {
		t.Fatal("\"strings\" already reachable before the edit; test setup is wrong")
	}
	if _, ok := snap2.Packages["strings"]; !ok {
		t.Fatal("\"strings\" not reachable after the edit; test setup is wrong")
	}
	s.setWorkspace(root, snap2)
	ws2 := s.workspace()

	if ws2.engine == ws1.engine {
		t.Error("setWorkspace reused engine even though the dependency set changed")
	}
	if ws2.graphSrc == ws1.graphSrc {
		t.Error("setWorkspace reused graphSrc even though the dependency set changed")
	}
	if _, err := ws2.engine.Get(context.Background(), greetGo); err != nil {
		t.Errorf("engine.Get(%s) on the rebuilt engine = %v, want nil", greetGo, err)
	}
}

// TestSetWorkspace_InFlightGetSurvivesReload is the flagship regression
// test for the production stall setWorkspace's Retire/reuse split fixes:
// before this change, every setWorkspace call (not only a go.mod-driven
// one) called the outgoing engine's Stop, which cancels the engine's own
// lifecycle ctx (Engine.ctx) that every Get flight — including one already
// in progress when the reload lands — runs on, surfacing as "server:
// checked package for ...: context canceled" for a request that was
// already past the point where a caller could retry it.
//
// Exercising a GENUINELY in-flight Get spanning the exact instant of a
// setWorkspace call would need a way to pause a recheck mid-flight (an
// importer gating hook, as internal/check's own
// TestEngine_Retire_LetsInFlightGetFinish uses via the unexported
// newTestEngineWithImporterHook constructor) — that seam is deliberately
// internal/check-only and not exposed to internal/server, and adding one
// solely for this test would be a production seam this change does not
// otherwise need. A timing-based race (start a goroutine's Get, sleep,
// then reload) would be flaky and is exactly the kind of test this
// codebase avoids.
//
// This test instead exercises the exact mechanism the bug hinged on: it
// completes a Get on the outgoing engine BEFORE the reload (so the engine
// is definitely warm and functional), forces setWorkspace onto its rebuild
// path (a dependency-set change, so the outgoing engine is genuinely
// retired, not reused), and then calls Get on that same, now-discarded
// engine again. A flight already in progress at the moment of Retire runs
// on the identical e.ctx a later Get on the same *Engine does — Retire
// does not distinguish between the two, and neither does Stop's
// cancellation — so this after-the-fact Get failing or succeeding is
// governed by precisely the same ctx state a genuinely concurrent one
// would have observed. Get returning a real result (not ctx.Err()) here is
// therefore equivalent-strength evidence that a concurrent one would too;
// internal/check's own TestEngine_Retire_LetsInFlightGetFinish separately
// covers that Retire, specifically, is what makes that true for a flight
// caught mid-recheck.
func TestSetWorkspace_InFlightGetSurvivesReload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	populateTempModule(t, root)

	snap1, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (first): %v", err)
	}
	s := newWorkspaceOnlyServerAt(t, root, snap1)
	oldEngine := s.workspace().engine

	greetGo := filepath.Join(root, "greet", "greet.go")
	if _, err := oldEngine.Get(context.Background(), greetGo); err != nil {
		t.Fatalf("warm-up Get on the pre-reload engine: %v", err)
	}

	// A newly reachable standard-library import forces setWorkspace's
	// rebuild path (see TestSetWorkspace_RebuildsEngineWhenDepsChanged),
	// so oldEngine is genuinely retired here, not reused.
	writeTempFile(t, filepath.Join(root, "greet"), "greet.go", "package greet\n\nimport \"strings\"\n\n// Hello returns a greeting.\nfunc Hello() string { return strings.ToUpper(\"hi\") }\n")
	snap2, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (second): %v", err)
	}
	s.setWorkspace(root, snap2)

	if s.workspace().engine == oldEngine {
		t.Fatal("setWorkspace reused oldEngine; test setup produced no dependency-set change")
	}

	if _, err := oldEngine.Get(context.Background(), greetGo); err != nil {
		t.Errorf("Get on the retired engine after reload = %v, want nil: Retire must not cancel Engine.ctx the way the old Stop-based behavior did, or every flight against the discarded engine — in-flight or not — fails with ctx.Err() instead of completing", err)
	}
}

// TestSetWorkspace_ConcurrentCallsStayConsistent guards against a logical
// race between two overlapping setWorkspace calls that both take the reuse
// path: without setWorkspaceMu serializing setWorkspace end to end, two
// calls can interleave their ws.graphSrc.Retarget and s.ws.Store steps,
// leaving the installed workspace's own snap (whatever s.ws.Store last
// installed) disagreeing with the shared, reused graphSrc's last Retarget
// target (whatever Retarget call happened to run last) — a genuine
// correctness bug, e.g. a hover resolving a package's file set against the
// wrong snapshot. This is a pure ordering race, not a data race in the
// -race-detectable sense (both graphSrc's index and s.ws are swapped via
// atomic.Pointer, each individual access is race-free); go test -race is
// run here only for its usual blanket coverage, not because it is expected
// to itself flag anything.
//
// Reproducing it needs genuine overlap right up to a call's own final
// Retarget/Store pair, not just any overlap somewhere in a long run: a
// free-running hammer of many goroutines looping thousands of times drifts
// out of phase, so by the time they all finish only one is typically still
// active and the tail is trivially self-consistent, without exercising the
// actual bug even though many mid-run overlaps did occur. This instead runs
// many independent rounds of exactly two goroutines released simultaneously
// via a barrier (one setting snapA, one setting snapB), checking the
// invariant after every round: synchronizing each round's start forces
// genuine overlap at exactly the moment that matters, so the check below —
// confirmed to fail within the first ~200 of 500 rounds before
// setWorkspaceMu existed, and to pass reliably after — is what actually
// exercises the race this test guards against.
//
// The invariant checked is that the installed workspace's own snap and its
// graphSrc agree on greet's GoFiles, since PackageForFile returns
// pkg.GoFiles straight from whichever *graphIndex Retarget last swapped in
// (see GraphSource.Retarget and PackageForFile). This deliberately does not
// use the added file itself as the probe: PackageForFile's directory
// fallback would resolve it to greet's package under EITHER snapshot (a new
// file landing in an already-known directory), masking the very
// inconsistency this test needs to catch.
func TestSetWorkspace_ConcurrentCallsStayConsistent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	populateTempModule(t, root)
	const greetPkgPath = "example.com/servermod/greet"
	greetGo := filepath.Join(root, "greet", "greet.go")

	snapA, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (A): %v", err)
	}
	writeTempFile(t, filepath.Join(root, "greet"), "greet_extra.go", "package greet\n\n// Extra is an additional exported function.\nfunc Extra() string { return \"extra\" }\n")
	snapB, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (B): %v", err)
	}
	if pkg, ok := snapA.Packages[greetPkgPath]; !ok || len(pkg.GoFiles) != 1 {
		t.Fatalf("snapA's greet package = %+v, want exactly greet.go; test setup is wrong", pkg)
	}
	if pkg, ok := snapB.Packages[greetPkgPath]; !ok || len(pkg.GoFiles) != 2 {
		t.Fatalf("snapB's greet package = %+v, want greet.go and greet_extra.go; test setup is wrong", pkg)
	}

	s := newWorkspaceOnlyServerAt(t, root, snapA)

	// See the barrier-per-round rationale in this test's own doc.
	const rounds = 500
	for round := range rounds {
		var start, wg2 sync.WaitGroup
		wg2.Add(2)
		start.Add(1)
		for i := range 2 {
			go func(i int) {
				defer wg2.Done()
				start.Wait()
				if (round+i)%2 == 0 {
					s.setWorkspace(root, snapA)
				} else {
					s.setWorkspace(root, snapB)
				}
			}(i)
		}
		start.Done()
		wg2.Wait()

		ws := s.workspace()
		wantPkg, ok := ws.snap.Packages[greetPkgPath]
		if !ok {
			t.Fatalf("round %d: greet package missing from the installed snapshot", round)
		}
		_, _, gotGoFiles, ok := ws.graphSrc.PackageForFile(greetGo)
		if !ok {
			t.Fatalf("round %d: ws.graphSrc.PackageForFile(greetGo) = ok false, want true", round)
		}
		if !sameGoFiles(wantPkg.GoFiles, gotGoFiles) {
			t.Fatalf("round %d: workspace inconsistent after concurrent setWorkspace calls: installed snap's greet.GoFiles=%v but graphSrc.PackageForFile returned %v; the reused graphSrc's last Retarget disagreed with the last installed snapshot", round, wantPkg.GoFiles, gotGoFiles)
		}
	}
}

// TestChangedExportSet covers changedExportSet's package-set diffing and
// reverse-dependency closure expansion against real graph.Load snapshots
// (graph.Snapshot.ClosureUnits' backing revDeps index is only populated by
// Load, not settable on a hand-built literal, so a real on-disk fixture is
// required to exercise closure propagation at all).
func TestChangedExportSet(t *testing.T) {
	tests := []struct {
		name string
		// setup runs before old is loaded, for a case that needs on-disk
		// state beyond writeExportSetModule's baseline before its "old"
		// snapshot even exists (only "removed file" needs this: a file
		// present for old and gone for snap, the reverse of "added file").
		setup func(t *testing.T, dir string)
		// mutate runs between the old and snap loads.
		mutate func(t *testing.T, dir string)
		want   []string
	}{
		{
			name:   "no changes",
			mutate: func(*testing.T, string) {},
			want:   nil,
		},
		{
			name: "added file, no importers",
			mutate: func(t *testing.T, dir string) {
				writeTempFile(t, filepath.Join(dir, "c"), "c_extra.go", "package c\n\n// CExtra is an additional exported function.\nfunc CExtra() {}\n")
			},
			want: []string{"example.com/exportset/c"},
		},
		{
			name: "removed file, no importers",
			setup: func(t *testing.T, dir string) {
				writeTempFile(t, filepath.Join(dir, "c"), "c_extra.go", "package c\n\n// CExtra is an additional exported function.\nfunc CExtra() {}\n")
			},
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "c", "c_extra.go")); err != nil {
					t.Fatalf("remove c_extra.go: %v", err)
				}
			},
			want: []string{"example.com/exportset/c"},
		},
		{
			name: "closure propagation: change in importee includes importer",
			mutate: func(t *testing.T, dir string) {
				writeTempFile(t, filepath.Join(dir, "a"), "a_extra.go", "package a\n\n// AExtra is an additional exported function.\nfunc AExtra() {}\n")
			},
			want: []string{"example.com/exportset/a", "example.com/exportset/b"},
		},
		{
			name: "package removed from snapshot",
			mutate: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "b")); err != nil {
					t.Fatalf("remove b: %v", err)
				}
			},
			want: []string{"example.com/exportset/b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeExportSetModule(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			old, err := graph.Load(graph.Options{Dir: dir}, "./...")
			if err != nil {
				t.Fatalf("graph.Load (old): %v", err)
			}

			tt.mutate(t, dir)

			snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
			if err != nil {
				t.Fatalf("graph.Load (snap): %v", err)
			}

			got := changedExportSet(old, snap)
			if !slices.Equal(got, tt.want) {
				t.Errorf("changedExportSet() = %v, want %v", got, tt.want)
			}
		})
	}
}

// writeExportSetModule creates a temp module with three packages: "a" (no
// imports), "b" (imports a, for closure-propagation coverage), and "c"
// (standalone, no imports and no importers, for GoFiles-diff coverage
// changedExportSet's closure walk must not spuriously enlarge). Returns the
// module's root directory.
func writeExportSetModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTempFile(t, dir, "go.mod", "module example.com/exportset\n\ngo 1.26\n")

	aDir := filepath.Join(dir, "a")
	if err := os.MkdirAll(aDir, 0o750); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	writeTempFile(t, aDir, "a.go", "package a\n\n// A does nothing.\nfunc A() {}\n")

	bDir := filepath.Join(dir, "b")
	if err := os.MkdirAll(bDir, 0o750); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}
	writeTempFile(t, bDir, "b.go", "package b\n\nimport \"example.com/exportset/a\"\n\n// B calls a.A.\nfunc B() { a.A() }\n")

	cDir := filepath.Join(dir, "c")
	if err := os.MkdirAll(cDir, 0o750); err != nil {
		t.Fatalf("mkdir c: %v", err)
	}
	writeTempFile(t, cDir, "c.go", "package c\n\n// C does nothing.\nfunc C() {}\n")

	return dir
}
