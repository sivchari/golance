package index

import (
	"context"
	"testing"
	"time"
)

// TestBuild_TestOnlyImportCycleDoesNotDeadlock is H11's scheduler-safety
// regression test: q imports p in production, and p's in-package test file
// imports q back — legal in Go (see graph.Package.TestImports's own doc)
// but a genuine cycle in the combined Imports+TestImports dependency graph
// schedulableDepsOf now folds into scheduling order. Before its
// position-based filtering (see its own doc), counting this TestImports
// edge in the scheduler's pendingDeps/remaining bookkeeping without also
// guarding against a cycle would make Build wait forever for a dependency
// that can never finish through this ordering, hanging indefinitely.
//
// p and q cannot both be given a combined key: p's own key needs q's export
// hash (via TestImports) but q's needs p's (via Imports) first — an
// inherent contradiction for these two specific packages, not a bug this
// fix can (or needs to) resolve. schedulableDepsOf's own doc explains the
// tradeoff this test pins: Build must still return promptly, with this one
// genuine cycle surfaced as each entangled package's own per-package error
// (Stats.Errors), never silently stale data and never a hang.
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
		if r.stats.Errors != 2 {
			t.Errorf("stats.Errors = %d, want 2 (both p and q are individually unresolvable through this one genuine cycle — see this test's own doc)", r.stats.Errors)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Build did not return within 10s: the scheduler likely deadlocked on the test-only import cycle")
	}
}
