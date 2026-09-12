package index

import (
	"sync/atomic"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/typecheck"
)

// scheduler drives Build's dependency-ordered, bounded-concurrency
// processing of root (workspace) packages: which packages are ready to
// run, and the reference-count bookkeeping that evicts a dependency from
// the shared typecheck.Cache once every package that imports it has
// finished. Non-root (stdlib/module) dependencies are excluded entirely:
// they are never scheduled as jobs of their own, only resolved on demand
// through the shared Importer's fallback ExportSource tier
// (internal/depexport.Cache — see its own package doc).
type scheduler struct {
	snap        *graph.Snapshot
	cache       *typecheck.Cache
	onEvicted   func(pkgPath string, cacheLen int)
	remaining   map[string]*int32 // fan-in counters, drive cache eviction
	pendingDeps map[string]*int32 // unfinished direct dependency counters, drive scheduling
	dependents  map[string][]string
	pos         map[string]int // import path -> index in snap.Order, see schedulableDepsOf
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
func newScheduler(snap *graph.Snapshot, cache *typecheck.Cache, onEvicted func(string, int)) (*scheduler, int) {
	pos := orderPositions(snap.Order)
	fanIn, dependents := computeFanIn(snap, pos)

	var total int
	pendingDeps := make(map[string]*int32, len(snap.Packages))
	for path, pkg := range snap.Packages {
		if !schedulableRoot(snap, pkg) {
			continue
		}
		total++
		var n int32
		for range schedulableDepsOf(snap, path, pkg, pos) {
			n++
		}
		v := n
		pendingDeps[path] = &v
	}

	remaining := make(map[string]*int32, len(fanIn))
	for path, n := range fanIn {
		v := n
		remaining[path] = &v
	}

	s := &scheduler{
		snap:        snap,
		cache:       cache,
		onEvicted:   onEvicted,
		remaining:   remaining,
		pendingDeps: pendingDeps,
		dependents:  dependents,
		pos:         pos,
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

// finish records that path has finished processing: it evicts any
// dependency whose last pending importer was path, and pushes any
// dependent whose last pending dependency was path onto ready. Call
// exactly once per package received from ready.
func (s *scheduler) finish(path string) {
	for _, dep := range schedulableDepsOf(s.snap, path, s.snap.Packages[path], s.pos) {
		ctr, ok := s.remaining[dep]
		if !ok {
			continue
		}
		if atomic.AddInt32(ctr, -1) == 0 {
			s.cache.Delete(dep)
			if s.onEvicted != nil {
				s.onEvicted(dep, s.cache.Len())
			}
		}
	}
	for _, dependent := range s.dependents[path] {
		if atomic.AddInt32(s.pendingDeps[dependent], -1) == 0 {
			s.ready <- dependent
		}
	}
	if atomic.AddInt32(&s.left, -1) == 0 {
		close(s.ready)
	}
}

// orderPositions returns each import path's index in order — snap.Order,
// see graph.Snapshot's own doc — for schedulableDepsOf to filter out an
// edge topoOrder's own rare cycle fallback could not fully satisfy before
// this scheduler ever tries to wait on it (see that function's doc).
func orderPositions(order []string) map[string]int {
	pos := make(map[string]int, len(order))
	for i, p := range order {
		pos[p] = i
	}
	return pos
}

// schedulableDepsOf returns pkg's direct workspace (root) dependencies
// eligible for scheduling: every entry in pkg.Imports and pkg.TestImports
// (an in-package test file's own extra imports — see its doc, and
// directDepImports' identical fold for [computeUnitKey]) naming a root
// package genuinely positioned before path in pos. An ordinary Imports edge
// always qualifies — Imports alone is guaranteed acyclic, so
// graph.Snapshot.Order always places it correctly — but a TestImports edge
// caught in the rare legal test-only cycle topoOrder's own fallback could
// not fully order is silently dropped here instead: counting it would make
// newScheduler's pendingDeps/remaining bookkeeping wait forever on a
// dependency that will never signal "finished" through this ordering,
// deadlocking Build entirely rather than merely reporting one package's own
// combined-key resolution as this run's error (see directDepImports' doc).
func schedulableDepsOf(snap *graph.Snapshot, path string, pkg *graph.Package, pos map[string]int) []string {
	var out []string
	for _, imports := range [][]string{pkg.Imports, pkg.TestImports} {
		for _, dep := range imports {
			d, ok := snap.Packages[dep]
			if !ok || !d.Root {
				continue
			}
			if pos[dep] >= pos[path] {
				continue
			}
			out = append(out, dep)
		}
	}
	return out
}

// computeFanIn returns, for every schedulable package in snap (see
// schedulableRoot), the number of direct root importers (fan-in), plus a
// dependents map from import path to the schedulable packages that import
// it directly (see schedulableDepsOf for which edges qualify). An external
// test package's own import of its base package (always a Root package) is
// what lets computeFanIn keep that base package's decoded *types.Package
// warm in the shared typecheck.Cache until the external test unit has also
// finished with it, and what makes the external test unit itself become
// ready once its base package finishes (see finish).
func computeFanIn(snap *graph.Snapshot, pos map[string]int) (fanIn map[string]int32, dependents map[string][]string) {
	fanIn = make(map[string]int32, len(snap.Packages))
	dependents = make(map[string][]string, len(snap.Packages))
	for path, pkg := range snap.Packages {
		if !schedulableRoot(snap, pkg) {
			continue
		}
		for _, dep := range schedulableDepsOf(snap, path, pkg, pos) {
			fanIn[dep]++
			dependents[dep] = append(dependents[dep], path)
		}
	}
	return fanIn, dependents
}
