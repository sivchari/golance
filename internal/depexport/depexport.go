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
package depexport

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
// golance build produced is never misread as current by a newer one.
const schemaVersion = 1

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
}

// Cache resolves and persists export data for non-root packages, backed by
// a machine-global store.CAS and a shared *depcheck.Provider for the
// declaration-only checks a cache miss requires. Implements
// typecheck.ExportSource. Safe for concurrent use. cas may be nil — every
// ExportData call still resolves correctly by checking pkgPath fresh via
// provider on every call, just without ever persisting or reusing the
// result — the same degraded-but-correct behavior an immutable-directory
// digest miss also falls back to when cas.Put fails; see NewCache's own
// doc for why this matters for a caller (e.g. a test, or a machine whose
// cache directory could not be created) that has no CAS to give it.
type Cache struct {
	cas      *store.CAS
	meta     depcheck.MetadataSource
	provider *depcheck.Provider

	goVersion  string
	buildFP    string
	goroot     string
	gomodcache string

	sf singleflight.Group
}

// NewCache returns a Cache resolving non-root package metadata via meta
// (typically depcheck.NewGraphMetadataSource over the same *graph.Snapshot
// provider itself resolves against — see depcheck.MetadataSource),
// declaration-only checking a cache miss via provider (sharing provider's
// own identity, LRU, and singleflight with any other caller resolving the
// same dependency — in particular internal/depcheck's own navigation
// consumers, when a Cache and a depcheck.Provider a session already
// maintains for navigation are deliberately the same instance; see
// internal/server's ensureDepProvider), and persisting the result in cas.
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
// as a real import edge; "unsafe" is resolved by depcheck.Provider itself
// and never reaches here (see depcheck.Provider.Package's own handling).
func (c *Cache) ExportData(pkgPath string) ([]byte, bool, error) {
	dir, _, _, ok := c.meta.Package(pkgPath)
	if !ok {
		return nil, false, nil
	}
	persist := c.cas != nil && c.immutable(dir)
	key := c.digest(pkgPath, dir)
	if persist {
		if blob, ok, err := c.cas.Get(context.Background(), key); ok || err != nil {
			return blob, ok, err
		}
	}

	v, err, _ := c.sf.Do(pkgPath, func() (any, error) {
		if persist {
			if blob, ok, err := c.cas.Get(context.Background(), key); ok || err != nil {
				return blob, err
			}
		}
		cp, err := c.provider.Package(context.Background(), pkgPath)
		if err != nil {
			return nil, fmt.Errorf("depexport: check %s: %w", pkgPath, err)
		}
		blob, err := typecheck.WriteExport(cp.Types(), c.provider.FileSet())
		if err != nil {
			return nil, fmt.Errorf("depexport: write export data for %s: %w", pkgPath, err)
		}
		if persist {
			if err := c.cas.Put(key, blob); err != nil {
				return nil, fmt.Errorf("depexport: persist export data for %s: %w", pkgPath, err)
			}
		}
		return blob, nil
	})
	if err != nil {
		return nil, false, err
	}
	blob, ok := v.([]byte)
	if !ok {
		return nil, false, fmt.Errorf("depexport: singleflight for %s returned %T, want []byte", pkgPath, v)
	}
	return blob, true, nil
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
