// Package depexport produces and persists gcexportdata-encoded export data
// for non-root (standard library and module-cache) dependency packages, by
// declaration-only source-type-checking them via internal/depcheck — never
// invoking the Go toolchain's own compiler — and caching the result in a
// machine-global, content-addressed store shared by every golance session
// and workspace on this machine.
//
// This replaces internal/graph's loadDepExportFiles/Snapshot.ExportFile
// (`go list -export`), which made the Go toolchain COMPILE every
// dependency package to obtain its export data: on a cold GOCACHE, that
// meant dozens of full ~400-500MB `compile` processes running at once for
// a large dependency closure (the field report this package exists to
// fix). Declaration-only type-checking (depcheck's IgnoreFuncBodies) is a
// small fraction of a full compile's cost, and once a dependency's export
// data has been produced ONCE on this machine — by any golance session,
// for any repository, ever — every later resolution of it is a single CAS
// read plus a gcexportdata.Read decode: no type-checking, no toolchain
// invocation, at all. gopls resolves these same dependencies the same way,
// by type-checking their real source rather than the compiler's export
// data (see internal/depcheck's own package doc); this package only adds
// the persistence internal/depcheck's small, per-process, in-memory LRU
// deliberately does not attempt (see depcheck.Provider's doc on that
// tradeoff).
//
// # Cache identity
//
// A blob is keyed by pkgPath + its resolved directory + the running Go
// toolchain's version + a build-flags fingerprint + schemaVersion — NOT by
// hashing the package's own file content. This is deliberately identity-
// based rather than content-based: a dependency's directory under GOROOT or
// the module cache (GOMODCACHE) is itself immutable for as long as that
// directory exists — the module cache is content-addressed by module
// version and read-only on disk, and internal/depcheck's own existing doc
// already relies on the identical assumption (see depcheck.Provider.check's
// doc: "module-cache and GOROOT files are immutable") — so the directory
// path alone already uniquely identifies whatever content will ever be
// found there, without reading it. Reading and hashing every dependency
// file's content on every build, merely to confirm nothing changed, would
// reintroduce a meaningful fraction of the I/O cost this package exists to
// remove, to guard against a case (a module-cache or GOROOT directory's
// content changing in place) that cannot happen. A dependency resolved
// OUTSIDE those two directories — a local `replace` directive pointing at a
// developer's own working copy, or GOPATH-mode source — is not
// identity-stable this way (its content genuinely can change between
// builds without its path changing), so Cache never persists an entry for
// one to the CAS: ExportData still resolves it correctly on every call, by
// asking the shared depcheck.Provider to check it fresh (itself cheap in
// practice: a `replace`-local package is a small, rare fraction of any
// real dependency closure), just without cross-process/cross-restart
// reuse.
//
// A package whose own check reported an error — most commonly one of its
// own transitive imports being momentarily unresolvable in the current
// graph, not a real defect in immutable dependency source — gets the exact
// same treatment regardless of directory identity (see
// depcheck.CheckedPackage.Incomplete): ExportData still returns its
// best-effort result for THIS call's own caller, but never persists it, so
// a transient resolution failure elsewhere in the workspace can never poison
// every repository on the machine with an incomplete blob for up to
// GCMaxAge.
package depexport

import (
	"context"
	"fmt"
	"go/token"
	"go/types"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// schemaVersion guards the CAS key against a change in this package's own
// digest composition or the export-data format Cache persists — bump
// whenever either changes, so a stale, differently-shaped blob a prior
// golance build produced is never misread as current by a newer one. Bumped
// to 2 for the fix closing the decode-fast-path export corruption (see
// internal/server.ensureDepProvider's doc): a machine whose CAS directory
// already holds a blob checkAndPersist wrote before that fix may hold one
// that gcexportdata.Write accepted but a later gcexportdata.Read cannot
// reliably decode — this bump forces every such pre-fix entry to be treated
// as a miss and rechecked fresh under the now-decode-disabled export
// Provider, rather than trusting whatever content-addressed hit it finds.
const schemaVersion = 2

// GC sizing for Cache's CAS directory, passed to (*store.CAS).MaybeGCAged
// by a caller that owns the *store.CAS itself (internal/server and
// cmd/golance — see their own wiring): unlike internal/store's per-repo
// GCInterval/GraceWindow, there is no UnitPointer-derived mark set this
// cache's blobs could ever be checked against (see (*store.CAS).GCAged's
// own doc) — GC here is age-only, and sized far more conservatively than a
// per-repo cache's, matching this package's own "computed once per machine
// ever" goal: a dependency that has not been resolved by ANY workspace on
// this machine in GCMaxAge is a strong signal it is genuinely no longer in
// use anywhere, not merely between one repository's ordinary builds.
const (
	// GCInterval bounds how often a MaybeGCAged caller actually walks this
	// cache's directory; a call in between is a cheap stamp-file stat.
	GCInterval = 7 * 24 * time.Hour
	// GCMaxAge is how long an unresolved blob survives before GC reclaims
	// it.
	GCMaxAge = 90 * 24 * time.Hour
)

// Options configures a Cache.
type Options struct {
	// GoVersion identifies the running Go toolchain in every digest this
	// Cache computes, so a toolchain upgrade (which can change a stdlib
	// package's own declarations, or this process's compiled-in
	// gcexportdata format) never reuses a blob a different toolchain
	// produced. Defaults to runtime.Version().
	GoVersion string
	// BuildFlagsFingerprint is folded into every digest this Cache
	// computes, so two workspaces built with different build tags/flags
	// never share a blob for the same import path. Optional; "" (the
	// common case today — see index.Options.BuildFlagsFingerprint's own
	// doc) folds in nothing extra.
	BuildFlagsFingerprint string
	// GOROOT and GOModCache identify the two directory trees Cache treats
	// as immutable (see the package doc's "Cache identity" section) and so
	// safe to persist by identity alone. Both default to the running
	// GO TOOLCHAIN's own values (defaultGOROOT's and defaultGOModCache's
	// `go env GOROOT`/`go env GOMODCACHE`, NOT runtime.GOROOT() — see
	// defaultGOROOT's own doc for why that distinction matters) when empty;
	// overridable so a test can point Cache at a throwaway module cache
	// under testdata instead of the real one.
	GOROOT     string
	GOModCache string
	// MemoizeForRun, if set, makes Cache remember every pkgPath it
	// successfully resolves (blob, complete, and ok — including a genuine
	// ok=false "not known to the graph" answer) for as long as this Cache
	// instance lives, regardless of persist/immutability — see Cache's own
	// doc for why this matters and why it defaults to off.
	MemoizeForRun bool
}

// Cache resolves and persists export data for non-root packages, backed by
// a machine-global store.CAS and a *depcheck.Provider for the
// declaration-only checks a cache miss requires. Implements
// typecheck.ExportSource. Safe for concurrent use. cas may be nil — every
// ExportData call still resolves correctly by checking pkgPath fresh via
// provider on every call, just without ever persisting or reusing the
// result — the same degraded-but-correct behavior an immutable-directory
// digest miss also falls back to when cas.Put fails; see NewCache's own
// doc for why this matters for a caller (e.g. a test, or a machine whose
// cache directory could not be created) that has no CAS to give it.
//
// provider should not have depcheck.Provider.SetExportSource called on it:
// checkAndPersist re-serializes provider.Package's result via
// typecheck.WriteExport to persist it, and decode output mixed into that
// result is one more way (on top of provider's own LRU eviction — see
// checkAndPersist's own doc for the confirmed mechanism, independent of
// SetExportSource either way) the resulting *types.Package graph can end up
// inconsistent. checkAndPersist's own round-trip self-check is what
// actually guarantees ExportData never returns or persists a blob that
// fails to decode, regardless of provider's configuration; keeping export
// production on its own, decode-disabled Provider (see
// internal/server.ensureDepProvider's own doc, the only production wiring
// where a Provider used for both navigation and export production was ever
// a real risk) is an additional safety margin on top of that, not the
// primary guarantee.
//
// Options.MemoizeForRun closes a SEPARATE, confirmed corruption class from
// either of the above: provider's own LRU (sized by RecommendedCap, bounded
// on purpose — see its own doc) is the ONLY identity source for a pkgPath
// this Cache is not persisting (a non-immutable directory — see the
// package doc's "Cache identity" section — most concretely a WORKSPACE
// package reached via the indexer's own #122-era withheld-export fallback,
// but any persist=false path qualifies), for EVERY caller across an entire
// Build/Reindex run, not just within one provider.Package call
// (closureScope's own protection — see typecheck.CheckScope's doc for the
// identical, already-fixed, single-call version of this). provider's own
// cross-call "dependency-based pin" (see depcheck.Provider.Delete's doc)
// only protects pkgPath for as long as SOME OTHER still-cached entry
// happens to claim it as a dependency; once that claiming entry itself is
// evicted — an ordinary event over a large build, and MORE frequent the
// more separate top-level callers a withheld root package's fallback now
// has (#122 multiplies exactly that) — pkgPath becomes eligible for
// eviction too, and the NEXT caller's resolve() re-checks it from scratch,
// producing a non-identical *types.Package for the same import path.
// Confirmed root cause of every observed WriteExport/ReadExport
// round-trip-decode panic in production so far (see
// typecheck.DuplicateImportPath's own doc): two dependents of a shared,
// widely-used cluster (github.com/microsoft/kiota-abstractions-go and a
// workspace package built on it) each independently re-checking it,
// producing two non-identical instances later woven into one consumer's
// own exported API. Memoizing every resolved pkgPath (bytes only, not the
// live *types.Package graph — a small, fixed cost per distinct path this
// run actually touches, unlike provider's own much larger live working
// set RecommendedCap bounds) for THIS Cache instance's own lifetime makes
// every resolve()for pkgPath within one run return byte-identical results
// regardless of how many separate callers ask, or how provider's own LRU
// has churned in between — closing the window without needing provider's
// LRU to hold anything longer than it already does.
//
// Left off by default (zero value) because Cache is also used by
// internal/server across a long-lived session spanning many file edits
// (see ensureDepProvider's own doc): a non-immutable (workspace) package's
// content genuinely CAN change between two ExportData calls there, and
// this Cache has no invalidation hook for an in-process memo the way
// typecheck.Cache's Delete gives its own decoded-package cache — memoizing
// unconditionally would serve stale bytes after an edit. internal/index's
// own Build/Reindex, which construct a brand-new Cache per call (see
// index.go/reindex.go), are the only current callers that opt in.
type Cache struct {
	cas      *store.CAS
	meta     depcheck.MetadataSource
	provider *depcheck.Provider

	goVersion  string
	buildFP    string
	goroot     string
	gomodcache string
	memoizeRun bool

	sf singleflight.Group

	memoMu sync.Mutex
	memo   map[string]memoEntry
}

// memoEntry is one pkgPath's remembered resolve() result, used only when
// Options.MemoizeForRun is set (see Cache's own doc).
type memoEntry struct {
	data     []byte
	complete bool
	ok       bool
}

// NewCache returns a Cache resolving non-root package metadata via meta
// (typically depcheck.NewGraphMetadataSource over the same *graph.Snapshot
// provider itself resolves against — see depcheck.MetadataSource),
// declaration-only checking a cache miss via provider, and persisting the
// result in cas. See Cache's own doc for provider's decode-disabled
// invariant.
func NewCache(cas *store.CAS, meta depcheck.MetadataSource, provider *depcheck.Provider, opts Options) *Cache {
	goVersion := opts.GoVersion
	if goVersion == "" {
		goVersion = runtime.Version()
	}
	goroot := opts.GOROOT
	if goroot == "" {
		goroot = defaultGOROOT()
	}
	gomodcache := opts.GOModCache
	if gomodcache == "" {
		gomodcache = defaultGOModCache()
	}
	return &Cache{
		cas: cas, meta: meta, provider: provider,
		goVersion: goVersion, buildFP: opts.BuildFlagsFingerprint,
		goroot: goroot, gomodcache: gomodcache,
		memoizeRun: opts.MemoizeForRun,
		memo:       make(map[string]memoEntry),
	}
}

// ExportData implements typecheck.ExportSource: resolves pkgPath's export
// data either from the persistent, machine-global CAS (a package this
// machine has already checked — this run, an earlier one, or a different
// workspace entirely) or by declaration-only source-checking it via c's
// depcheck.Provider and persisting the result for next time (see the
// package doc's "Cache identity" section for when persisting is safe at
// all). ok is false only when pkgPath is not known to c's MetadataSource —
// should not happen for anything an Importer actually asks for, since it
// only ever asks for a package the same *graph.Snapshot already reported
// as a real import edge.
//
// pkgPath must never be "unsafe": types.Unsafe has no source of its own and
// gcexportdata.Write (via WriteExport, below) panics unconditionally trying
// to serialize it. typecheck.Importer.ImportFrom special-cases "unsafe"
// before ever reaching an ExportSource (see its own doc) — this Cache's
// only current caller — so this method never actually sees it; that
// special case, not anything in this package or in depcheck.Provider
// (which only special-cases "unsafe" for ITS OWN recursive import
// resolution, a separate call path that never reaches ExportData at all),
// is what upholds this precondition.
func (c *Cache) ExportData(pkgPath string) ([]byte, bool, error) {
	blob, _, ok, err := c.resolve(pkgPath)
	return blob, ok, err
}

// ExportDataComplete is ExportData's richer variant, additionally reporting
// whether the returned bytes came from a complete check (a CAS hit is
// always complete — checkAndPersist only ever persists a blob when its own
// check was) — see depcheck.CheckedPackage.Incomplete's identical meaning.
// Implements depcheck.ExportSource (a structural interface depcheck itself
// declares, so depcheck need not import this package — see its own doc):
// depcheck.Provider's ctxImporter consumes this to propagate incompleteness
// through its decode fast path for a TRANSITIVE import exactly the way a
// full recursive check already does for a directly-requested one.
func (c *Cache) ExportDataComplete(pkgPath string) (data []byte, complete, ok bool, err error) {
	return c.resolve(pkgPath)
}

// ExportDataFromCache resolves pkgPath's export data ONLY from the
// persistent CAS, never falling through to checkAndPersist's from-source
// check on a miss — unlike ExportData/ExportDataComplete, which always do.
// ok is false whenever no CAS entry already exists for pkgPath: because
// pkgPath's directory is not one Cache ever persists to at all (see the
// package doc's "Cache identity" section — a local `replace`-directive
// dependency, most commonly), because the CAS itself is nil (see NewCache's
// own doc), or because it is simply not there yet. Used by a caller that
// must never let a single query trigger an unbounded, recursive from-source
// check of pkgPath's whole transitive closure — see
// internal/server.depCacheHolder.importer's cold-index-build gate, the only
// current caller: while the facts index is still building, the closure this
// package's own indexer subprocess is already checking would otherwise be
// checked a second time, in-process, by the server itself (the field
// symptom this exists to fix — see internal/server's own doc on the
// gate for the full chain).
func (c *Cache) ExportDataFromCache(pkgPath string) (data []byte, ok bool, err error) {
	if c.cas == nil {
		return nil, false, nil
	}
	dir, _, _, mok := c.meta.Package(pkgPath)
	if !mok || !c.immutable(dir) {
		return nil, false, nil
	}
	key := c.digest(pkgPath, dir)
	blob, hit, err := c.cas.Get(context.Background(), key)
	if err != nil {
		return nil, false, err
	}
	return blob, hit, nil
}

// resolve is ExportData's and ExportDataComplete's shared implementation.
// See Cache's own doc for the MemoizeForRun tier this checks first and
// populates last.
func (c *Cache) resolve(pkgPath string) (data []byte, complete, ok bool, err error) {
	if c.memoizeRun {
		c.memoMu.Lock()
		e, known := c.memo[pkgPath]
		c.memoMu.Unlock()
		if known {
			return e.data, e.complete, e.ok, nil
		}
	}

	data, complete, ok, err = c.resolveUnmemoized(pkgPath)
	// A transient failure (e.g. a momentarily unresolvable transitive
	// import — see checkAndPersist's own doc) is never memoized: the NEXT
	// call may legitimately succeed once whatever made this one fail
	// resolves, the same leniency checkAndPersist already extends to an
	// Incomplete-but-otherwise-usable result (which IS memoized: Incomplete
	// is not an error here).
	if c.memoizeRun && err == nil {
		c.memoMu.Lock()
		if e, known := c.memo[pkgPath]; known {
			// A concurrent caller's own resolve already landed first;
			// keep it, not this one — two independently-resolved results
			// for the same path is exactly the divergence MemoizeForRun
			// exists to prevent, so whichever is memoized first wins for
			// every caller for the rest of this run.
			data, complete, ok = e.data, e.complete, e.ok
		} else {
			c.memo[pkgPath] = memoEntry{data: data, complete: complete, ok: ok}
		}
		c.memoMu.Unlock()
	}
	return data, complete, ok, err
}

// resolveUnmemoized is resolve's own pre-MemoizeForRun implementation.
func (c *Cache) resolveUnmemoized(pkgPath string) (data []byte, complete, ok bool, err error) {
	dir, _, _, ok := c.meta.Package(pkgPath)
	if !ok {
		return nil, false, false, nil
	}
	persist := c.cas != nil && c.immutable(dir)
	key := c.digest(pkgPath, dir)
	if persist {
		if blob, hit, err := c.cas.Get(context.Background(), key); hit || err != nil {
			return blob, true, hit, err
		}
	}

	v, err, _ := c.sf.Do(pkgPath, func() (any, error) {
		return c.checkAndPersist(pkgPath, persist, key)
	})
	if err != nil {
		return nil, false, false, err
	}
	res, ok := v.(exportResult)
	if !ok {
		return nil, false, false, fmt.Errorf("depexport: singleflight for %s returned %T, want exportResult", pkgPath, v)
	}
	return res.blob, res.complete, true, nil
}

// exportResult is checkAndPersist's singleflight payload: the resolved blob
// plus whether the check that produced it was complete (see
// ExportDataComplete's doc).
type exportResult struct {
	blob     []byte
	complete bool
}

// checkAndPersist is resolve's singleflight-guarded slow path: a CAS hit
// that raced ahead of this call while it waited to run (persist only),
// otherwise a fresh declaration-only check via c.provider, persisted to the
// CAS when persist allows it and the check was not Incomplete (see the
// field's own doc).
func (c *Cache) checkAndPersist(pkgPath string, persist bool, key uint64) (exportResult, error) {
	if persist {
		if blob, ok, err := c.cas.Get(context.Background(), key); ok || err != nil {
			return exportResult{blob: blob, complete: true}, err
		}
	}
	cp, err := c.provider.Package(context.Background(), pkgPath)
	if err != nil {
		return exportResult{}, fmt.Errorf("depexport: check %s: %w", pkgPath, err)
	}
	if dup := typecheck.DuplicateImportPath(cp.Types()); dup != "" {
		return exportResult{}, fmt.Errorf("depexport: export data for %s would reference two non-identical packages both named %q (declaration-only check reported: %s): see typecheck.DuplicateImportPath's doc", pkgPath, dup, firstErrorOrNone(cp))
	}
	blob, err := writeExportRecovered(cp.Types(), c.provider.FileSet())
	if err != nil {
		return exportResult{}, fmt.Errorf("depexport: write export data for %s (declaration-only check reported: %s): %w", pkgPath, firstErrorOrNone(cp), err)
	}
	// gcexportdata.Write happily serializes a *types.Package graph that
	// contains two non-identical *types.Package instances for the same
	// import path — c.provider's own LRU eviction can produce exactly that
	// within a single recursive check, when a widely-shared dependency gets
	// evicted and re-checked from scratch partway through resolving cp's own
	// transitive imports (see depcheck.DefaultCap's own doc on why a cap
	// too small for the closure being checked thrashes; RecommendedCap
	// narrows but does not eliminate this for a single closure larger than
	// its own ceiling). The resulting blob writes without error but cannot
	// be reliably read back: gcimporter panics decoding it ("internal error
	// ... invalid memory address or nil pointer dereference", recovered
	// into an opaque error) — confirmed reproducible with c.provider's
	// decode fast path OFF just as readily as with it on, so this check
	// catches the corruption regardless of which mechanism produced it,
	// current or future. A throwaway fset/cache keeps this self-check from
	// touching any cache a real caller shares. firstErrorOrNone(cp) is
	// folded into the wrapped error so a caller (and this Cache's own
	// server-side logging) can see WHAT the declaration-only check itself
	// reported, not just that the resulting blob failed to round-trip.
	if _, err := typecheck.ReadExport(blob, token.NewFileSet(), pkgPath, typecheck.NewCache()); err != nil {
		return exportResult{}, fmt.Errorf("depexport: export data for %s does not round-trip decode (declaration-only check reported: %s): %w", pkgPath, firstErrorOrNone(cp), err)
	}
	// cp.Incomplete (see its own doc) means pkgPath's check — or a
	// transitive import's — reported at least one error, e.g. one of its
	// own dependencies was momentarily unresolvable in the current graph:
	// blob still reflects go/types' best-effort result (correct for THIS
	// call's own caller, see typecheck.NewImporter's identical
	// resolves-but-does-not-persist contract), but persisting it to the
	// machine-global CAS would let every repository on this machine keep
	// being served that same degraded result for up to GCMaxAge, long
	// after the transient condition that produced it is gone.
	complete := !cp.Incomplete()
	if persist && complete {
		if err := c.cas.Put(key, blob); err != nil {
			return exportResult{}, fmt.Errorf("depexport: persist export data for %s: %w", pkgPath, err)
		}
	}
	return exportResult{blob: blob, complete: complete}, nil
}

// firstErrorOrNone returns cp.FirstError(), or "(none)" when empty — a
// declaration-only check can be Incomplete purely transitively (an import
// it resolved was itself already Incomplete — see
// depcheck.CheckedPackage.Incomplete's own doc), in which case cp's own
// FirstError has nothing to report even though Incomplete is true.
func firstErrorOrNone(cp *depcheck.CheckedPackage) string {
	if e := cp.FirstError(); e != "" {
		return e
	}
	return "(none)"
}

// writeExportRecovered calls typecheck.WriteExport, converting a panic into
// a returned error instead of crashing checkAndPersist's own caller: a
// declaration-only check's error recovery can leave a declaration reachable
// from cp's exported API — most concretely a generic instantiation's
// synthesized method — with an invalid or missing *types.Signature, which
// gcexportdata's writer has no guard for (confirmed reproducible: a panic
// inside internal/gcimporter's non-shallow doDecl, go/types.(*Signature).Recv
// on a nil receiver, not an internalError-typed panic, so gcimporter's own
// recover re-panics it — see internal/index.writeAndValidateExport's
// identical recovery for the same confirmed mechanism on the root-package
// export path).
func writeExportRecovered(pkg *types.Package, fset *token.FileSet) (blob []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			// See internal/index.writeAndValidateExport's identical split:
			// the stack is logged (otherwise unrecoverable once this defer
			// returns), not folded into err, which stays a one-line
			// diagnostic sample.
			log.Printf("depexport: write export data for %s panicked: %v\n%s", pkg.Path(), r, debug.Stack())
			err = fmt.Errorf("write export data for %s panicked: %v", pkg.Path(), r)
		}
	}()
	return typecheck.WriteExport(pkg, fset)
}

// immutable reports whether dir falls under c's GOROOT or GOModCache — the
// two directory trees this Cache treats as content-stable by path alone
// (see the package doc's "Cache identity" section).
func (c *Cache) immutable(dir string) bool {
	return underDir(dir, c.goroot) || underDir(dir, c.gomodcache)
}

// underDir reports whether path is root itself or falls somewhere under
// it, purely lexically (via filepath.Rel) — root's own callers (immutable)
// always pass an already-resolved, absolute directory on both sides, so no
// symlink resolution is attempted here.
func underDir(path, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// digest returns the CAS key for pkgPath's export data at dir, folding in
// c's Go version and build-flags fingerprint plus schemaVersion — never
// pkgPath/dir's own file content (see the package doc's "Cache identity"
// section for why that is sound for a caller c.immutable(dir) allows to
// persist at all; a digest is still computed and used for the in-flight
// singleflight/CAS-hit path even when persist is false, so a concurrent
// ExportData call for the same non-immutable pkgPath still collapses onto
// one check via sf.Do — the key itself is simply never Put).
func (c *Cache) digest(pkgPath, dir string) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s\x00", schemaVersion, c.goVersion, c.buildFP, pkgPath, dir)
	return h.Sum64()
}

// defaultGOROOT resolves the USER's actual toolchain GOROOT — deliberately
// NOT runtime.GOROOT(), which returns the GOROOT baked into THIS BINARY's
// own build (wrong the moment the binary runs on a different machine than
// it was built on, e.g. a goreleaser-built release binary: SA1019 flags
// runtime.GOROOT as deprecated for exactly this reason since Go 1.24).
// Mirrors internal/langfeat's identical goroot resolution (`go env GOROOT`,
// the officially supported way to locate it, falling back to $GOROOT when
// the go binary is not on PATH) rather than importing that package's
// unexported helper: langfeat is a higher-level UI package depending on
// internal/check/depcheck, not a dependency this low-level package should
// take on. Computed once per process for the same reason
// defaultGOModCache is: a `go env` subprocess per Cache construction would
// add a spawn to every first call for no benefit, and GOROOT cannot change
// without a process restart in practice.
var defaultGOROOT = sync.OnceValue(func() string {
	if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root
		}
	}
	return os.Getenv("GOROOT")
})

// defaultGOModCache returns the running environment's module cache
// directory: $GOMODCACHE if set, otherwise `go env GOMODCACHE` (which also
// sees a value set via `go env -w`, unlike the environment variable alone),
// falling back to $GOPATH/pkg/mod (Go's own documented default) if even
// that fails — e.g. no `go` binary on PATH, which should not happen inside
// golance's own process (it IS a Go program) but is guarded rather than
// assumed. Computed once per process: the module cache location cannot
// change without a process restart in practice, and a Cache-per-call
// re-invocation of `go env` would add a subprocess spawn to every first
// Cache construction for no benefit.
var defaultGOModCache = sync.OnceValue(func() string {
	if v := os.Getenv("GOMODCACHE"); v != "" {
		return v
	}
	if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		home, _ := os.UserHomeDir()
		gopath = filepath.Join(home, "go")
	}
	return filepath.Join(gopath, "pkg", "mod")
})
