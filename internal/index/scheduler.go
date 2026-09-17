package index

import (
	"sync/atomic"

	"github.com/sivchari/golance/internal/graph"
)

// scheduler drives Build's dependency-ordered, bounded-concurrency
// processing of root (workspace) packages: which packages are ready to run,
// tracked purely over direct root-to-root import edges (schedulableDepsOf).
// It no longer ref-counts anything for cache eviction — internal/index.Build
// now bounds the shared decode cache's memory by rotating to a fresh
// generation once it grows past budget (see generations), not by deleting
// entries mid-run: a dependency's references can reach a package this
// scheduler never sees an edge to at all, through another already-decoded
// package's own export data, so a scheduler-driven refcount can never
// safely decide when an entry is no longer needed (see
// internal/typecheck.Cache's own doc).
type scheduler struct {
	snap        *graph.Snapshot
	pendingDeps map[string]*int32 // unfinished direct dependency counters, drive scheduling
	dependents  map[string][]string
	ready       chan string
	left        int32
}

// schedulableRoot reports whether pkg is one of the (up to two per
// directory) units Build processes as an independent job: an ordinary Root
// (workspace) package, or its external "_test"-suffixed test package (see
// isExternalTestOfRoot) — a second, distinct pkgPath sharing the same
// directory. graph.go's fromPackages already gives that package its own
// real pkgPath, so no (dir, variant) key is needed the way
// internal/check.Engine's unitKey uses one; this predicate plays the same
// role schedulableRoot(pkg) == pkg.Root alone used to.
func schedulableRoot(snap *graph.Snapshot, pkg *graph.Package) bool {
	return pkg.Root || isExternalTestOfRoot(snap, pkg)
}

// newScheduler prepares a scheduler over snap's schedulable packages (see
// schedulableRoot) and seeds its ready channel with every package that has
// no unfinished dependency (a zero in-degree in that subgraph). total is the
// number of packages to process; a scheduler for total == 0 has nothing to
// do.
func newScheduler(snap *graph.Snapshot) (*scheduler, int) {
	dependents := computeDependents(snap)

	var total int
	pendingDeps := make(map[string]*int32, len(snap.Packages))
	for path, pkg := range snap.Packages {
		if !schedulableRoot(snap, pkg) {
			continue
		}
		total++
		var n int32
		for range schedulableDepsOf(snap, path, pkg) {
			n++
		}
		v := n
		pendingDeps[path] = &v
	}

	s := &scheduler{
		snap:        snap,
		pendingDeps: pendingDeps,
		dependents:  dependents,
		ready:       make(chan string, total),
		left:        int32(total),
	}
	if total == 0 {
		close(s.ready)
		return s, 0
	}
	for path, dep := range pendingDeps {
		if atomic.LoadInt32(dep) == 0 {
			s.ready <- path
		}
	}
	return s, total
}

// finish records that path has finished processing: it pushes any dependent
// whose last pending dependency was path onto ready. Call exactly once per
// package received from ready.
func (s *scheduler) finish(path string) {
	for _, dependent := range s.dependents[path] {
		if atomic.AddInt32(s.pendingDeps[dependent], -1) == 0 {
			s.ready <- dependent
		}
	}
	if atomic.AddInt32(&s.left, -1) == 0 {
		close(s.ready)
	}
}

// schedulableDepsOf returns pkg's direct workspace (root) dependencies
// eligible for scheduling: every entry in pkg.Imports and pkg.TestImports
// (an in-package test file's own extra imports — see its doc, and
// directDepImports' identical fold for [computeUnitKey]) naming a root
// package snap.Before reports as genuinely positioned before path. An
// ordinary Imports edge always qualifies — Imports alone is guaranteed
// acyclic, so graph.Snapshot.Order always places it correctly — but a
// TestImports edge caught in the rare legal test-only cycle topoOrder's own
// fallback could not fully order is silently dropped here instead: counting
// it would make newScheduler's pendingDeps bookkeeping wait forever on a
// dependency that will never signal "finished" through this ordering,
// deadlocking Build entirely. directDepImports applies this exact same
// snap.Before filter for its own, different reason (see its doc), so a
// dropped edge here is also never required for [computeUnitKey] — dropping
// it never surfaces as this run's error the way it used to before that
// filter existed there too.
func schedulableDepsOf(snap *graph.Snapshot, path string, pkg *graph.Package) []string {
	var out []string
	for _, imports := range [][]string{pkg.Imports, pkg.TestImports} {
		for _, dep := range imports {
			d, ok := snap.Packages[dep]
			if !ok || !d.Root {
				continue
			}
			if !snap.Before(dep, path) {
				continue
			}
			out = append(out, dep)
		}
	}
	return out
}

// computeDependents returns, for every schedulable package in snap (see
// schedulableRoot), the schedulable packages that import it directly (see
// schedulableDepsOf for which edges qualify) — finish's own lookup table for
// which dependents to make ready once path itself finishes. An external
// test package's own import of its base package (always a Root package) is
// what makes the external test unit itself become ready once its base
// package finishes.
func computeDependents(snap *graph.Snapshot) map[string][]string {
	dependents := make(map[string][]string, len(snap.Packages))
	for path, pkg := range snap.Packages {
		if !schedulableRoot(snap, pkg) {
			continue
		}
		for _, dep := range schedulableDepsOf(snap, path, pkg) {
			dependents[dep] = append(dependents[dep], path)
		}
	}
	return dependents
}
