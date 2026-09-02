package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// TestEnsureDepProvider_ReusedWhenDepsUnchanged verifies ensureDepProvider
// returns the identical *depcheck.Provider across two snapshots whose
// non-workspace (dependency) package sets are the same — the fix for the
// production stall documented on ensureDepProvider: setWorkspace runs on
// every graph revalidation, not only at initialize, and discarding
// depProvider's whole type-check cache every time forced the next
// dependency-facing navigation query to re-check its import closure cold.
func TestEnsureDepProvider_ReusedWhenDepsUnchanged(t *testing.T) {
	snap1 := &graph.Snapshot{Packages: map[string]*graph.Package{
		"example.com/root": {ImportPath: "example.com/root", Root: true, Dir: "/root"},
		"fmt":              {ImportPath: "fmt", Dir: "/goroot/src/fmt", GoFiles: []string{"print.go"}},
	}}
	// snap2 differs only in the ROOT package's own metadata (as an ordinary
	// workspace edit would produce) — the dependency set itself (fmt) is
	// byte-identical.
	snap2 := &graph.Snapshot{Packages: map[string]*graph.Package{
		"example.com/root": {ImportPath: "example.com/root", Root: true, Dir: "/root", GoFiles: []string{"edited.go"}},
		"fmt":              {ImportPath: "fmt", Dir: "/goroot/src/fmt", GoFiles: []string{"print.go"}},
	}}

	s := &Server{}
	p1 := s.ensureDepProvider(snap1)
	if p1 == nil {
		t.Fatal("ensureDepProvider returned nil")
	}
	p2 := s.ensureDepProvider(snap2)
	if p1 != p2 {
		t.Error("ensureDepProvider rebuilt the Provider even though the dependency set was unchanged")
	}
}

// TestEnsureDepProvider_RebuildsWhenDepsChanged verifies ensureDepProvider
// builds a fresh *depcheck.Provider once the dependency set itself differs
// (here, a new package became reachable) — the correctness half of the same
// fix: reuse must never survive an actual go.mod/go.sum-driven change.
func TestEnsureDepProvider_RebuildsWhenDepsChanged(t *testing.T) {
	snap1 := &graph.Snapshot{Packages: map[string]*graph.Package{
		"fmt": {ImportPath: "fmt", Dir: "/goroot/src/fmt", GoFiles: []string{"print.go"}},
	}}
	snap2 := &graph.Snapshot{Packages: map[string]*graph.Package{
		"fmt":     {ImportPath: "fmt", Dir: "/goroot/src/fmt", GoFiles: []string{"print.go"}},
		"strings": {ImportPath: "strings", Dir: "/goroot/src/strings", GoFiles: []string{"strings.go"}},
	}}

	s := &Server{}
	p1 := s.ensureDepProvider(snap1)
	p2 := s.ensureDepProvider(snap2)
	if p1 == p2 {
		t.Error("ensureDepProvider reused the Provider even though the dependency set changed")
	}
}

// TestSetWorkspace_DepProviderCacheSurvivesReload is the end-to-end version
// of TestEnsureDepProvider_ReusedWhenDepsUnchanged, against a real
// setWorkspace call and a real depcheck.Provider check: priming the
// Provider's cache for "strings" via one setWorkspace-installed workspace,
// then calling setWorkspace again with a freshly (but identically) loaded
// snapshot must leave that cache warm — a second Package("strings") call
// must be served from the LRU (Checked() unchanged), not re-check it cold.
func TestSetWorkspace_DepProviderCacheSurvivesReload(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap1, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (first): %v", err)
	}

	s := &Server{logger: newTestLogger(t)}
	s.setWorkspace(root, snap1)

	ctx := context.Background()
	if _, err := s.workspace().depProvider.Package(ctx, "strings"); err != nil {
		t.Fatalf("Package(strings) on the first workspace: %v", err)
	}
	checkedBefore := s.workspace().depProvider.Checked()
	if checkedBefore == 0 {
		t.Fatal("Checked() == 0 after a cold Package call; test setup is wrong")
	}
	providerBefore := s.workspace().depProvider

	// Simulate a reload triggered by an ordinary edit (e.g. a new file
	// landing in an already-known package directory — see
	// needsGraphReload): a fresh graph.Load of the SAME module, producing a
	// distinct *graph.Snapshot with the identical dependency set.
	snap2, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (second): %v", err)
	}
	if snap1 == snap2 {
		t.Fatal("graph.Load returned the same *graph.Snapshot twice; test setup is wrong")
	}
	s.setWorkspace(root, snap2)

	if s.workspace().depProvider != providerBefore {
		t.Fatal("setWorkspace rebuilt depProvider across a reload with an unchanged dependency set")
	}
	if _, err := s.workspace().depProvider.Package(ctx, "strings"); err != nil {
		t.Fatalf("Package(strings) on the second workspace: %v", err)
	}
	if got := s.workspace().depProvider.Checked(); got != checkedBefore {
		t.Errorf("Checked() = %d after the reload, want unchanged %d (a warm cache hit, not a cold re-check)", got, checkedBefore)
	}
}
