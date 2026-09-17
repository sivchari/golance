package index

import (
	"go/token"
	"sync"

	"github.com/sivchari/golance/internal/typecheck"
)

// defaultDecodeCacheBudget is cacheGenerations' default per-generation byte
// budget (see Options.DecodeCacheBudget), in decoded export-data blob bytes.
//
// Matches internal/xref's and internal/depcheck's 256MiB caps rather than
// internal/server depCacheHolder's 512MiB: a Build decodes deep
// (self-contained) export data for a whole workspace closure, and blob bytes
// understate the decoded heap by roughly an order of magnitude. On a
// 2560-root monorepo 512MiB never rotated (408MiB decoded) while 128MiB
// rotated 10 times, paying re-decode time for a modestly smaller heap;
// 256MiB rotates a few times.
const defaultDecodeCacheBudget = 256 << 20

// cacheGeneration is one append-only (fset, cache, importer) triple: a
// single CheckPackage call binds to exactly one generation for parse,
// decode, and facts extraction, so every package it resolves — directly, or
// transitively via another already-decoded package's own export data —
// shares one *types.Package instance per import path (see
// typecheck.Cache's own doc for why that invariant requires never deleting
// an entry while some other, still-live check may reference it).
type cacheGeneration struct {
	fset  *token.FileSet
	cache *typecheck.Cache
	imp   *typecheck.Importer
}

// cacheGenerations hands out the current generation, rotating to a fresh
// one once its cache grows past budget — the same whole-pair-swap pattern
// internal/server's depCacheHolder, internal/xref's Resolver, and
// internal/depcheck's exportResolver already use for their own caches (see
// each package's own doc) — instead of per-entry removal mid-build, which
// cannot safely bound memory without risking the identity split
// internal/typecheck.Cache's own doc describes. A generation an
// in-flight check is still holding a local reference to is never mutated
// once rotated past; it is simply dropped, along with everything it
// cached, once nothing references it any longer — an ordinary Go GC, not an
// explicit refcount.
type cacheGenerations struct {
	mu      sync.Mutex
	cur     *cacheGeneration
	budget  int64
	newGen  func() *cacheGeneration
	created int // number of generations created so far, see count
}

// newCacheGenerations returns a cacheGenerations seeded with one eagerly
// created generation from newGen, rotating to a fresh one via newGen
// whenever the current one's cache exceeds budget bytes.
func newCacheGenerations(budget int64, newGen func() *cacheGeneration) *cacheGenerations {
	return &cacheGenerations{cur: newGen(), budget: budget, newGen: newGen, created: 1}
}

// acquire returns the current generation, rotating to a fresh one first if
// its cache has grown past budget. Callers must call this after any
// blocking wait (e.g. a semaphore) that could stall long enough for the
// budget to be crossed by other concurrent work in the meantime, so a check
// never binds to a generation already past budget at the moment it starts.
func (g *cacheGenerations) acquire() *cacheGeneration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur.cache.Bytes() > g.budget {
		g.cur = g.newGen()
		g.created++
	}
	return g.cur
}

// count returns the number of generations created so far, including the
// first — Stats.CacheGenerations' source.
func (g *cacheGenerations) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.created
}

// currentBytes returns the current generation's decoded blob bytes (see
// typecheck.Cache.Bytes).
func (g *cacheGenerations) currentBytes() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cur.cache.Bytes()
}
