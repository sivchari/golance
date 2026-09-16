package depcheck

import (
	"context"
	"go/token"
	"go/types"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/sivchari/golance/internal/typecheck"
)

// ExportSource resolves pkgPath's persisted, gcexportdata-encoded export
// data, computing and persisting it via a fresh check if not already known
// — satisfied by *depexport.Cache's ExportDataComplete method without
// depcheck importing that package (depexport already imports depcheck, for
// depcheck.Provider/MetadataSource/CheckedPackage; the dependency can only
// run one direction). complete reports whether the check that produced data
// (this call's own fresh check, or a previously persisted one — a CAS entry
// is only ever persisted when complete) finished without error, the
// identical meaning CheckedPackage.Incomplete already carries: ctxImporter's
// decode fast path (see Provider.exportResolver) folds this into the
// CURRENT check's own Incomplete result exactly like resolving the same
// import via a full recursive check already does (see
// ctxImporter.importIncomplete's doc).
type ExportSource interface {
	ExportDataComplete(pkgPath string) (data []byte, complete, ok bool, err error)
}

// exportDecodeCap bounds exportResolver's decode cache, measured by
// typecheck.Cache's own Bytes (see decode) — a serialized-blob byte sum,
// not the live *types.Package object graph those blobs decode into (see
// Bytes's own doc). Once past it, r's decode cache is discarded and
// replaced with an empty one — a coarse, whole-cache reset rather than a
// per-package LRU, mirroring internal/server.maxDepCacheBytes's identical
// tradeoff for the same reason: a decoded *types.Package is far cheaper per
// entry than a full CheckedPackage (no AST, no statement-level Info — see
// CheckedPackage's own doc), so bounding by total decoded bytes lets this
// cache hold many more distinct packages than lru's own entry-count cap
// (DefaultCap/RecommendedCap) before paying any reset cost, while keeping
// worst-case memory bounded independent of workspace size — the same goal
// lru's own cap already serves for full CheckedPackage entries.
//
// This must stay a generous, blob-byte cap rather than a heap-scaled one: a
// single resolve() call's caller needs every package the transitive
// closure it is decoding depends on to stay resident and identity-stable
// simultaneously (see the doc above r.pkgs/r.cache's use as gcexportdata's
// shared imports map). Scaling the comparison up (as an earlier revision
// briefly did, comparing Bytes()*10 against this same cap) shrinks the
// effective threshold to a fraction of what its own name promises; for any
// dependency closure whose blob bytes exceed that shrunk threshold — e.g.
// connect-go's generated + grpc + protobuf closure — the eviction fires
// again on every subsequent decode inside the same resolution, wiping
// r.pkgs (and so every already-decoded package's *types.Package identity)
// before it can finish, so the resolution never converges: an unbounded
// decode-discard-redecode loop instead of the one-time cache reset this cap
// is meant to be (confirmed by reproducing the hang against the connect
// generic-field regression this cap change was meant to fix, then reverting
// it). The cold-start server-RSS blowup that motivated scaling by a
// decoded-heap multiplier in the first place is bounded by the cold-index
// gate (see depCacheHolder.importer's coldGateSource), not by shrinking
// this cache's cap.
const exportDecodeCap = 256 * 1024 * 1024 // 256MiB blob-byte cap

// exportResolver decodes an ExportSource's persisted bytes into a shared
// *token.FileSet, giving ctxImporter a cheap alternative to a full recursive
// source-check for a package reached only as someone else's TRANSITIVE
// import — never for a pkgPath a caller directly requested via
// Provider.Package/PackageWithBodies, which always stays source-checked in
// lru/fullLRU (see ctxImporter.ImportFrom's own doc for why these tiers
// stay strictly separate: decoded and source-checked instances for the SAME
// pkgPath must never both feed a single check's own recursive import walk).
//
// Decoding is cheap relative to a full parse-and-type-check — this is the
// entire point: a widely-shared package evicted from the small
// CheckedPackage LRU mid-closure is normally re-parsed and re-type-checked
// from scratch on every subsequent importer that reaches it again (the
// production regression this exists to fix); resolving it here instead
// costs one ExportSource lookup (typically a CAS read, or a decode of bytes
// this same resolver already holds) plus one gcexportdata decode.
//
// One instance per Provider (see Provider.SetExportSource), guarded by its
// own mutex entirely separate from lru/fullLRU's own locking, with its own
// singleflight group so concurrent ctxImporters resolving the same
// transitive path collapse onto one decode.
//
// fset is a *token.FileSet fully independent of the owning Provider's own
// p.fset: a decoded package is never a Decl/DeclAt target (ctxImporter.
// ImportFrom's own doc — a DIRECT Provider.Package/PackageWithBodies
// request always source-checks into p.fset instead, byte-exact), so
// nothing needs its positions to share p.fset's coordinate space. Sharing
// p.fset here instead of owning a dedicated one (as this package's own
// prior revision did) corrupted every decode it touched: p.fset is also
// the SAME fset ExportSource's own reentrant check parses path's source
// files into (depexport.Cache.checkAndPersist calls Provider.Package,
// which parses into p.fset, immediately before this resolver's caller
// hands that same p.fset to gcexportdata for the ENCODE); asking
// gcexportdata to then DECODE that freshly-written blob back into the very
// fset its own source positions were just parsed into registers path's
// files into p.fset a second time and reliably crashes the unified
// importer (observed: gcexportdata/gcimporter panics with "internal error
// ... invalid memory address or nil pointer dereference" on almost every
// decode against a real dependency closure). See typecheck.Cache's own
// doc — "must discard the Cache and its fset together, never
// independently" — and internal/server.depCacheHolder, which already
// pairs its own dedicated fset with its typecheck.Cache for the identical
// reason; this resolver now follows the same pattern instead of borrowing
// p.fset.
type exportResolver struct {
	src ExportSource

	mu       sync.Mutex
	fset     *token.FileSet
	cache    *typecheck.Cache
	pkgs     map[string]*types.Package
	complete map[string]bool
	resets   int64 // count of resetLocked calls triggered by a decode failure; see Provider.ExportResolverResets

	sf singleflight.Group
}

// newExportResolver returns an exportResolver decoding via src into its
// own dedicated *token.FileSet (see exportResolver's doc for why this must
// never be the owning Provider's own p.fset).
func newExportResolver(src ExportSource) *exportResolver {
	return &exportResolver{
		src: src, fset: token.NewFileSet(),
		cache: typecheck.NewCache(), pkgs: make(map[string]*types.Package), complete: make(map[string]bool),
	}
}

// resetLocked discards r's entire decode generation (fset, cache, pkgs,
// complete) and replaces it with a fresh, empty one — the same coarse,
// whole-generation reset exportDecodeCap's own byte-cap trigger already
// performs inline in resolve (see its own doc for the concurrent-decode
// caveat this shares), factored out so resolve's decode-failure path below
// can reuse it identically. r.mu must already be held.
func (r *exportResolver) resetLocked() {
	r.fset = token.NewFileSet()
	r.cache = typecheck.NewCache()
	r.pkgs = make(map[string]*types.Package)
	r.complete = make(map[string]bool)
	r.resets++
}

// decodeResult is resolve's singleflight payload: ok is false only when
// src has no data for path at all (see ExportSource's own doc — should not
// happen for anything ctxImporter actually asks for, but resolve's caller
// falls back to a full check either way, see ImportFrom).
type decodeResult struct {
	pkg      *types.Package
	complete bool
	ok       bool
}

// get returns path's already-decoded package and completeness, if r already
// holds one, without touching r.src at all.
func (r *exportResolver) get(path string) (pkg *types.Package, complete, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pkg, ok = r.pkgs[path]
	if !ok {
		return nil, false, false
	}
	return pkg, r.complete[path], true
}

// resolve decodes path via r's ExportSource into r's own dedicated fset
// (see exportResolver's doc for why this must never be the owning
// Provider's own p.fset), sharing identity with every other transitive
// import decoded through r for as long as r's cache stays under
// exportDecodeCap (see the coarse reset below, which discards r.fset
// together with r.cache — a concurrent decode for a DIFFERENT path racing
// a reset can land in the about-to-be-discarded generation instead of the
// fresh one; narrow in practice and accepted for the identical reason
// internal/server.depCacheHolder.FileSet's own coarse-reset window is).
// ok is false, with no error, when r itself is nil (no ExportSource
// configured — see Provider.SetExportSource) or src has no data for path.
func (r *exportResolver) resolve(ctx context.Context, path string) (pkg *types.Package, complete, ok bool, err error) {
	if r == nil || r.src == nil {
		return nil, false, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, false, err
	}
	if pkg, complete, ok := r.get(path); ok {
		return pkg, complete, true, nil
	}

	v, err, _ := r.sf.Do(path, func() (any, error) {
		if pkg, complete, ok := r.get(path); ok {
			return decodeResult{pkg: pkg, complete: complete, ok: true}, nil
		}
		data, complete, ok, err := r.src.ExportDataComplete(path)
		if err != nil {
			return nil, err
		}
		if !ok {
			return decodeResult{}, nil
		}

		r.mu.Lock()
		if r.cache.Bytes() > exportDecodeCap {
			r.resetLocked()
		}
		fset := r.fset
		cache := r.cache
		r.mu.Unlock()

		pkg, err := typecheck.ReadExport(data, fset, path, cache)
		if err != nil {
			// data itself already round-tripped cleanly in isolation before
			// ever reaching r (see internal/depexport.checkAndPersist's own
			// self-check) — a failure here means r's own decode generation,
			// shared across every transitive import resolve has ever
			// decoded, currently holds an entry data's own blob references
			// that violates gcexportdata.Read's contract ("imports[path]
			// does not exist, or exists but is incomplete" — see ReadExport's
			// doc), the identical staleness typecheck.Importer.decode's own
			// self-heal guards against. Nothing ever pins an entry in r's
			// own generation (closureScope deliberately never extends here —
			// see exportResolver's own doc), so resetLocked's discard-
			// everything behavior needs no pin-awareness: every concurrent
			// resolve racing this one either already captured its own
			// fset/cache pair above (and keeps decoding into the
			// about-to-be-discarded generation, the same narrow, accepted
			// window resolve's own doc already names for the byte-cap
			// trigger) or has not yet, and lands in the fresh one instead —
			// no live *types.Package graph anywhere depends on r's own
			// generation staying stable the way a live CheckScope does for
			// typecheck.Cache, so this cannot split identity for any
			// concurrent resolve the way a reset mid-CheckPackage-call
			// could.
			r.mu.Lock()
			r.resetLocked()
			fset = r.fset
			cache = r.cache
			r.mu.Unlock()

			pkg, err = typecheck.ReadExport(data, fset, path, cache)
			if err != nil {
				return nil, err
			}
		}

		r.mu.Lock()
		r.pkgs[path] = pkg
		r.complete[path] = complete
		r.mu.Unlock()
		return decodeResult{pkg: pkg, complete: complete, ok: true}, nil
	})
	if err != nil {
		return nil, false, false, err
	}
	res, ok := v.(decodeResult)
	if !ok || !res.ok {
		return nil, false, false, nil
	}
	return res.pkg, res.complete, true, nil
}

// SetExportSource installs src as p's persistent export-data source for
// TRANSITIVE import resolution (see ExportSource's and exportResolver's own
// docs): once set, an import ctxImporter reaches only through someone
// else's check — never a pkgPath a caller of Package/PackageWithBodies
// itself asked for — first tries decoding src's persisted bytes instead of
// a full recursive source-check, falling back to one only when src has
// nothing persisted for that path yet (src's own ExportDataComplete
// contract already performs that fallback check and persists its result —
// see depexport.Cache.ExportDataComplete's doc — so p's own check for a
// given pkgPath still runs at most once machine-wide, not once per importer
// that reaches it in a large closure).
//
// Callers wire this after constructing both p and src, since src (typically
// a *depexport.Cache) itself needs p at its own construction — see
// internal/server.ensureDepProvider's wiring, the only production caller.
// Not calling this at all (the zero value, nil) keeps Provider's
// pre-existing behavior unchanged: every import, at every depth, is fully
// source-checked, exactly as every existing depcheck test that never calls
// this continues to exercise.
func (p *Provider) SetExportSource(src ExportSource) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exports = newExportResolver(src)
}

// exportResolverFor returns p's current exportResolver, nil if
// SetExportSource was never called.
func (p *Provider) exportResolverFor() *exportResolver {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exports
}

// recordDecoded increments the count of ImportFrom calls ctxImporter served
// via the decode fast path (see Decoded).
func (p *Provider) recordDecoded() {
	p.mu.Lock()
	p.decoded++
	p.mu.Unlock()
}

// Decoded returns the number of times ctxImporter.ImportFrom resolved a
// transitive import via the decode fast path (exportResolver) instead of a
// full recursive source-check. Test-observability hook, mirroring Checked:
// the regression this package's ExportSource support fixes shows up as
// Checked() growing far past the closure's own distinct package count while
// a large fraction of repeat transitive touches land here instead.
func (p *Provider) Decoded() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.decoded
}

// ExportResolverResets returns the number of times p's exportResolver
// self-healed a gcexportdata.Read failure by discarding its entire decode
// generation and retrying (see exportResolver.resolve's own doc). Zero if
// SetExportSource was never called (no exportResolver exists at all).
// Test-observability hook, mirroring Decoded.
func (p *Provider) ExportResolverResets() int64 {
	p.mu.Lock()
	r := p.exports
	p.mu.Unlock()
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resets
}
