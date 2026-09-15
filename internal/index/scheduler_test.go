package index

import (
	"context"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/store"
)

// TestBuild_TestOnlyImportCycleDoesNotDeadlock is H11's scheduler-safety
// regression test: q imports p in production, and p's in-package test file
// imports q back — legal in Go (see graph.Package.TestImports's own doc)
// but a genuine cycle in the combined Imports+TestImports dependency graph
// schedulableDepsOf folds into scheduling order. Before its position-based
// filtering (see its own doc), counting this TestImports edge in the
// scheduler's pendingDeps/remaining bookkeeping without also guarding
// against a cycle would make Build wait forever for a dependency that can
// never finish through this ordering, hanging indefinitely.
//
// p and q are NOT symmetrically entangled despite the apparent cycle: only
// q's own production Imports edge to p is a real build-order requirement;
// p's back-edge exists solely through its own _test.go file. graph.Snapshot's
// second, Imports-only topoOrder pass (see its own doc) resolves that real
// edge correctly — p before q — leaving only the test-only back-edge
// genuinely unresolvable, which directDepImports' own snap.Before filter
// (see its doc) now excludes from p's combined key instead of failing p
// outright. Both packages index successfully; q, processed after p, gets
// full fidelity including its own test-inclusive pass, while p's own
// combined key simply never folds in q's export hash (see directDepImports'
// doc for that bounded, documented tradeoff).
func TestBuild_TestOnlyImportCycleDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/schedulecycle\n\ngo 1.23\n")
	writeFile(t, dir, "p/p.go", `// Package p is a leaf in production.
package p

// P returns 1.
func P() int { return 1 }
`)
	writeFile(t, dir, "p/p_test.go", `package p

import (
	"testing"

	"example.com/schedulecycle/q"
)

// TestP imports q, which (in production) imports p — the legal
// in-package-test-only back-edge this fixture exercises.
func TestP(t *testing.T) {
	if P() != 1 || q.Q() != 2 {
		t.Fatal("unexpected result")
	}
}
`)
	writeFile(t, dir, "q/q.go", `// Package q depends on p in production.
package q

import "example.com/schedulecycle/p"

// Q returns p.P() + 1.
func Q() int { return p.P() + 1 }
`)

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)

	type result struct {
		stats Stats
		err   error
	}
	done := make(chan result, 1)
	go func() {
		stats, err := Build(context.Background(), snap, db, cas, &Options{})
		done <- result{stats, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Build: %v (Build's own error is reserved for a db-wide failure — a per-package error like this cycle must never become one, see Build's own doc)", r.err)
		}
		if r.stats.Errors != 0 {
			t.Errorf("stats.Errors = %d, want 0 (the test-only back-edge alone must never fail either package — see this test's own doc)", r.stats.Errors)
		}
		if _, err := db.GetUnit(context.Background(), store.Hash("example.com/schedulecycle/p")); err != nil {
			t.Errorf("GetUnit(p): %v (p must still be indexed despite the excluded test-only back-edge)", err)
		}
		pID := findSymbolByName(t, db, cas, "example.com/schedulecycle/p", "P")
		var qRefsP bool
		viewFacts(t, db, cas, "example.com/schedulecycle/q", func(v *store.View) {
			for _, r := range v.RefsTo(pID) {
				if r.ToPkgHash() == store.Hash("example.com/schedulecycle/p") {
					qRefsP = true
				}
			}
		})
		if !qRefsP {
			t.Error("q has no ref resolving to p.P's SymbolID; the real, one-directional production edge must still resolve correctly")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Build did not return within 10s: the scheduler likely deadlocked on the test-only import cycle")
	}
}
