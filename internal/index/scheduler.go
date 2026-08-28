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
// through the shared Importer's export-file fallback.
type scheduler struct {
	snap        *graph.Snapshot
	cache       *typecheck.Cache
	onEvicted   func(pkgPath string, cacheLen int)
	remaining   map[string]*int32 // fan-in counters, drive cache eviction
	pendingDeps map[string]*int32 // unfinished direct dependency counters, drive scheduling
	dependents  map[string][]string
	ready       chan string
	left        int32
}

// newScheduler prepares a scheduler over snap's root packages and seeds its
// ready channel with every package that has no unfinished dependency (a
// zero in-degree in the root-only subgraph). total is the number of root
// packages to process; a scheduler for total == 0 has nothing to do.
func newScheduler(snap *graph.Snapshot, cache *typecheck.Cache, onEvicted func(string, int)) (*scheduler, int) {
	fanIn, dependents := computeFanIn(snap)

	var total int
	pendingDeps := make(map[string]*int32, len(snap.Packages))
	for path, pkg := range snap.Packages {
		if !pkg.Root {
			continue
		}
		total++
		var n int32
		for _, dep := range pkg.Imports {
			if d, ok := snap.Packages[dep]; ok && d.Root {
				n++
			}
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
	for _, dep := range s.snap.Packages[path].Imports {
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

// computeFanIn returns, for every root (workspace) package in snap, the
// number of direct root importers (fan-in), plus a dependents map from
// import path to the root packages that import it directly.
func computeFanIn(snap *graph.Snapshot) (fanIn map[string]int32, dependents map[string][]string) {
	fanIn = make(map[string]int32, len(snap.Packages))
	dependents = make(map[string][]string, len(snap.Packages))
	for path, pkg := range snap.Packages {
		if !pkg.Root {
			continue
		}
		for _, dep := range pkg.Imports {
			d, ok := snap.Packages[dep]
			if !ok || !d.Root {
				continue
			}
			fanIn[dep]++
			dependents[dep] = append(dependents[dep], path)
		}
	}
	return fanIn, dependents
}
