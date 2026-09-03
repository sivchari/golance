// Package graph loads the workspace's Go import graph via go/packages and
// keeps it available as a Snapshot: package metadata, a topological build
// order, and reverse-dependency closures. It deliberately never requests
// type or syntax information (NeedTypes/NeedSyntax) — that is internal/typecheck's
// job, done on demand per package.
//
// Load requests test variants (Config.Tests, mirroring gopls's own
// `-test=true`; see research-gopls-dependency-nav.md's Q1) so the loaded
// graph's transitive closure is complete in the same sense gopls's is:
// every package a _test.go file can import — including stdlib packages no
// production file in the workspace imports at all, such as "testing", and
// module dependencies like testify — gets a real Package entry with
// Dir/GoFiles/ExportFile, not just the packages production code reaches.
// Without this, internal/typecheck's export-data importer has nothing to
// resolve "testing" from, and every feature touching a _test.go file's use
// of it (hover, definition, diagnostics) fails outright.
//
// go list's own test-variant identifiers ("p", "p [q.test]", "q_test
// [q.test]"; see Q1) never leak into this package's own Package.ImportPath
// keys: fromPackages folds each variant back onto its real import path (a
// distinct PkgPath for an external test package, the same PkgPath as its
// base for an in-package one) so every other Snapshot consumer keeps
// working against plain, unadorned import paths exactly as before.
package graph

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// exportFileMode is the minimal go/packages.Load mode that resolves export
// data without also compiling anything Root (see loadMode's doc): used by
// loadDepExportFiles' batched dependency-export load right after an
// ordinary Load, and by reloadExportFile to recover a single package's
// export data later (see Snapshot.ExportFile's doc for when that runs).
const exportFileMode = packages.NeedName | packages.NeedExportFile

// loadMode is the main go/packages.Load mode graph requests for the whole
// workspace. NeedTypes and NeedSyntax must never be added here: loading
// type/syntax information for the whole workspace is exactly the cost this
// package exists to avoid. NeedForTest is required for fromPackages to tell
// a test variant apart from an ordinary package at all: go/packages leaves
// Package.ForTest permanently "" — even with Config.Tests: true and even
// though the underlying `go list -json` output does carry it — unless this
// bit is requested (see golist.go's addFields, gated on cfg.Mode&NeedForTest).
//
// NeedExportFile is deliberately NOT included here, unlike through v0.5.0:
// requesting it makes the underlying `go list` run with `-export=true`,
// which COMPILES every matched package's export data rather than just
// resolving metadata — for "./..." over a whole workspace, that means
// compiling every one of possibly thousands of Root packages. golance never
// reads a Root package's own ExportFile (Root packages are type-checked
// straight from source — see internal/index/scheduler.go's doc and
// internal/check); only a NON-root (stdlib/module-cache) dependency's
// export data is ever consulted (internal/typecheck.ExportFileSource, wired
// through workspace.depCache/depProvider). GOCACHE is keyed by absolute
// path, so on a brand-new git worktree this -export compilation is a 100%
// cache miss across the whole workspace — the dominant cost of a cold
// worktree open (routinely minutes on a large monorepo), for data nothing
// ever reads. Load instead resolves export data only for the packages that
// can actually need it, via loadDepExportFiles.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedImports |
	packages.NeedDeps | packages.NeedForTest

// Package is one node in the import graph: a single Go package as reported
// by go/packages, without any type or syntax information.
type Package struct {
	ImportPath string `json:"importPath"`
	// Name is the package's declared name (e.g. "yaml" for the import path
	// "gopkg.in/yaml.v2", which can differ from its last path segment).
	// go/packages.NeedName already resolves this as part of loadMode, so it
	// costs nothing beyond what Load already pays for — see
	// internal/langfeat's unimported-completion candidate index, the reason
	// this field exists.
	Name    string   `json:"name,omitempty"`
	Dir     string   `json:"dir"`
	GoFiles []string `json:"goFiles"`
	Imports []string `json:"imports"` // import paths of direct dependencies present in the graph, from this package's own production (non-test) files only — see TestImports
	// TestImports is the EXTRA import paths this package's in-package
	// _test.go files pull in beyond Imports — e.g. "testing", or a test
	// helper module like testify, or even a workspace package only a test
	// depends on. It comes from go list's own "p [p.test]" variant of this
	// package (see the package doc's Q1 reference) and is kept separate
	// from Imports, never merged into it, for one load-bearing reason:
	// Imports must stay a DAG for topoOrder (Snapshot.Order) to succeed,
	// but a test-only edge can legally point back at a package that
	// (in production) imports this one — Go itself only permits that
	// specific shape of import cycle across the test/production split (go
	// list represents it as two distinct nodes, "p" and "p [p.test]", for
	// exactly this reason; see fromPackages). Merging TestImports into
	// Imports would turn that legal pattern into a spurious cycle and make
	// Load fail outright. Nothing downstream consumes this field yet in
	// this phase (see internal/graph's package doc); it exists so the data
	// is not thrown away, for a later phase's closure-based invalidation to
	// pick up deliberately.
	TestImports []string `json:"testImports,omitempty"`
	// ForTest is the real import path of the package this entry is a test
	// variant OF, non-empty only for an externally-keyed test-only node
	// fromPackages materializes under its own distinct PkgPath (an external
	// "_test"-suffixed test package, or the rare "helper rebuilt for a
	// test context" case — see fromPackages' second branch); always empty
	// for an ordinary package, mirroring go/packages.Package.ForTest 1:1
	// for exactly the entries that keep it. Consumers that enumerate
	// snap.Packages for anything resembling "a real, normally-importable
	// workspace-adjacent package" (e.g. internal/check.GraphSource and
	// internal/server's own file/dir index, and unimported-completion
	// candidates) must skip a non-empty ForTest: such a node can share its
	// Dir with the ordinary package it is a variant of (an external test
	// package always does — same directory, different PkgPath), and can
	// never legitimately be imported by anything, so treating it as an
	// ordinary directory occupant would misroute a new file landing in
	// that directory or offer an unimportable path as a completion
	// candidate.
	ForTest    string `json:"forTest,omitempty"`
	ExportFile string `json:"exportFile,omitempty"`
	Root       bool   `json:"root,omitempty"` // matched a Load pattern directly, as opposed to being pulled in only as a dependency
}

// Snapshot is an immutable view of the import graph: every loaded package,
// a topological build order (dependencies before dependents), and reverse
// dependency closures.
type Snapshot struct {
	Packages map[string]*Package `json:"packages"`
	Order    []string            `json:"order"`

	dir        string              // working directory to re-run packages.Load from, for ExportFile's recovery path
	buildFlags []string            // forwarded to packages.Config.BuildFlags on that same recovery path
	revDeps    map[string][]string // import path -> direct importers present in the graph

	// recovered caches, per import path, the outcome of ExportFile's
	// reloadExportFile fallback: either a recovered export file path, or —
	// stored as the recoveryFailed sentinel — that recovery was already
	// attempted and failed. Either way, a later call for the same package
	// after the same stat failure reuses the cached outcome instead of
	// paying for another recovery subprocess; without caching the failure
	// case too, a query touching a package whose export data can never be
	// recovered (e.g. a genuinely broken dependency) would re-run the
	// expensive go/packages.Load on every single call for it. A Snapshot is
	// published once (via Load or LoadCache) and read concurrently by many
	// request handlers from then on (see internal/server's
	// atomic.Pointer[workspace]), so this cache — the one place ExportFile
	// writes anything back — must be synchronized itself rather than a
	// plain map; the Packages field above stays exactly as published, never
	// mutated in place. The cache lives only as long as this Snapshot: a
	// later revalidateGraph/setWorkspace publishes a fresh Snapshot with an
	// empty recovered map, so a dependency's export data that becomes
	// recoverable again (e.g. after `go build` repopulates GOCACHE) is
	// retried on the next graph reload rather than staying permanently
	// failed.
	recovered sync.Map // string (import path) -> string (recovered export file path, or recoveryFailed)
}

// recoveryFailed is the sentinel Snapshot.recovered stores for an import
// path whose reloadExportFile fallback has already been tried and failed —
// never a value reloadExportFile could itself return, since a real export
// file path is never empty and never starts with NUL.
const recoveryFailed = "\x00recovery-failed"

// Options configures a Load call.
type Options struct {
	Dir        string   // working directory packages.Load runs from
	BuildFlags []string // forwarded to packages.Config.BuildFlags
	Offline    bool     // inject GOPROXY=off, forbidding network module downloads
}

// Load runs go/packages.Load for patterns and returns the resulting
// Snapshot. Loading a dependency not yet in the module cache can trigger a
// `go mod download` under the hood (the same as `go build`/gopls); pass
// Offline to forbid that.
func Load(opts Options, patterns ...string) (*Snapshot, error) {
	cfg := &packages.Config{
		Mode:       loadMode,
		Dir:        opts.Dir,
		BuildFlags: opts.BuildFlags,
		Env:        os.Environ(),
		// Tests requests go list's test variants ("-test=true"), the
		// package doc's Q1 reference — without it, a _test.go file's
		// test-only imports (starting with "testing" itself) are simply
		// never part of the loaded graph at all, since go list never
		// builds a package specially "for" a test unless asked to.
		Tests: true,
	}
	if opts.Offline {
		cfg.Env = append(cfg.Env, "GOPROXY=off")
	}
	roots, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("graph: load packages: %w", err)
	}

	// rootSet marks the import paths that matched a Load pattern directly
	// — i.e. workspace packages, the indexer's roots. With Tests: true,
	// roots can also contain, per matched directory, its in-package test
	// variant ("p [p.test]": ForTest == p's own PkgPath), its external test
	// variant ("p_test [p.test]": a distinct PkgPath), and go list's
	// synthesized "p.test" test-executable "main" package — none of those
	// three are real, independently-indexable workspace packages (see
	// fromPackages), so only the ordinary ForTest == "" entry marks its
	// PkgPath as Root.
	rootSet := make(map[string]bool, len(roots))
	for _, p := range roots {
		if p.ForTest == "" && !isSyntheticTestBinary(p) {
			rootSet[p.PkgPath] = true
		}
	}

	var all []*packages.Package
	packages.Visit(roots, func(p *packages.Package) bool {
		all = append(all, p)
		return true
	}, nil)

	pkgs := fromPackages(all, rootSet)
	loadDepExportFiles(opts, pkgs)
	return newSnapshot(pkgs, opts.Dir, opts.BuildFlags)
}

// loadDepExportFiles resolves export data for every NON-root package in
// pkgs (the stdlib/module-cache dependencies internal/typecheck.
// ExportFileSource actually needs — see loadMode's doc for why Root
// packages are excluded) via one batched go/packages.Load, populating each
// Package.ExportFile in place. Root packages' own ExportFile is left "",
// exactly as it is never read.
//
// This runs as a second `go list -export` limited to the dependency import
// paths already known from pkgs, rather than the whole "./..." pattern:
// module-cache and GOROOT paths are stable across every worktree of a
// repository (unlike the workspace's own absolute path), so GOCACHE is
// warm for them after the very first build on the machine — this is cheap,
// not the cost loadMode's doc describes.
//
// Best-effort: if the batched load itself fails outright (e.g. a transient
// build error unrelated to any specific package), pkgs simply keeps
// whatever ExportFile values it already had (i.e. none), and
// Snapshot.ExportFile's own per-package reloadExportFile fallback recovers
// each one lazily on its first actual use instead of leaving the whole
// Snapshot permanently degraded.
func loadDepExportFiles(opts Options, pkgs map[string]*Package) {
	var patterns []string
	for path, pkg := range pkgs {
		if !pkg.Root {
			patterns = append(patterns, path)
		}
	}
	if len(patterns) == 0 {
		return
	}
	sort.Strings(patterns)

	cfg := &packages.Config{
		Mode:       exportFileMode,
		Dir:        opts.Dir,
		BuildFlags: opts.BuildFlags,
		Env:        os.Environ(),
	}
	if opts.Offline {
		cfg.Env = append(cfg.Env, "GOPROXY=off")
	}
	loaded, err := packages.Load(cfg, patterns...)
	if err != nil {
		return
	}
	for _, p := range loaded {
		if pkg, ok := pkgs[p.PkgPath]; ok && p.ExportFile != "" {
			pkg.ExportFile = p.ExportFile
		}
	}
}

// fromPackages converts go/packages results into the graph's own
// lightweight, JSON-serializable Package representation, keyed by real
// import path — never by a go list test-variant ID (see the package doc).
// rootSet marks the import paths that matched a Load pattern directly.
//
// pkgs, with Tests: true, can list up to four go/packages.Package values
// per workspace directory that declares tests: the ordinary package
// ("p", ForTest == ""), its in-package test variant ("p [p.test]", ForTest
// == p's own PkgPath), its external test variant ("p_test [p.test]", a
// distinct PkgPath, ForTest == p's PkgPath), and go list's synthesized
// "p.test" test-executable driver (Name == "main", excluded entirely — see
// isSyntheticTestBinary). fromPackages runs in two passes over pkgs: the
// first materializes one canonical Package per ordinary (ForTest == "")
// entry; the second folds each test variant into the graph — an
// in-package variant's EXTRA imports onto its already-materialized base
// (see Package.TestImports), an external (or otherwise distinctly-keyed)
// variant as its own canonical Package. This order requires no
// pkgs-iteration-order assumption: every in-package variant's base was
// already materialized in the first pass regardless of pkgs' order, since
// no test variant shares a pass.
//
// Intermediate test variants (ITVs — syntactically identical to an
// ordinary variant but carrying a *different* package's import view, e.g.
// "net/http [net/url.test]") are not modeled separately here, mirroring
// gopls's own documented policy (Q1, "why we mostly ignore intermediate
// test variants"): if an ITV is the only variant fromPackages ever sees for
// its PkgPath (its ordinary, ForTest == "" sibling never separately
// reachable), it is folded into the canonical map like any other
// externally-keyed variant would be — an accepted, documented soundness
// gap identical in kind to gopls's, not a new one. golance's stated Phase 1
// scope (definition/hover navigation) does not require full ITV modeling.
func fromPackages(pkgs []*packages.Package, rootSet map[string]bool) map[string]*Package {
	out := make(map[string]*Package, len(pkgs))
	for _, p := range pkgs {
		if p.ForTest != "" || isSyntheticTestBinary(p) {
			continue
		}
		out[p.PkgPath] = &Package{
			ImportPath: p.PkgPath,
			Name:       p.Name,
			Dir:        p.Dir,
			GoFiles:    p.GoFiles,
			Imports:    importPaths(p),
			ExportFile: p.ExportFile,
			Root:       rootSet[p.PkgPath],
		}
	}

	for _, p := range pkgs {
		if p.ForTest == "" || isSyntheticTestBinary(p) {
			continue
		}
		if p.PkgPath == p.ForTest {
			// Ordinary in-package test variant ("p [p.test]"): same
			// PkgPath as the base package already materialized above.
			// Record only what it adds beyond that base package's own
			// production Imports (see Package.TestImports's doc for why
			// this is never merged into Imports itself).
			if base, ok := out[p.PkgPath]; ok {
				base.TestImports = extraImports(base.Imports, importPaths(p))
			}
			continue
		}
		// Every other test-only variant carries its own distinct PkgPath:
		// the external "_test"-suffixed test package ("p_test [p.test]"),
		// or — rarely — a dependency go list chose to build specially
		// "for" a test context even though it does not itself import the
		// package under test (go/packages' own ID-grammar comment, Q1:
		// "...plus any helpers used by the external test q_test, typically
		// including testing and all its dependencies"). Either way its
		// PkgPath can never collide with an ordinary package materialized
		// above (a real import path is never test-variant-suffixed on top
		// of another real one), so it becomes its own canonical entry,
		// never Root (see rootSet's doc): "cheap" completeness for
		// anything that resolves it by that exact path, though nothing in
		// golance does today. If the same PkgPath is reached from more
		// than one q.test context, the first one visited wins — its
		// Dir/GoFiles/ExportFile do not vary by context; only its build's
		// internal type identity would, which this plain, non-ITV-aware
		// graph does not model (see the ITV note above).
		if _, exists := out[p.PkgPath]; exists {
			continue
		}
		out[p.PkgPath] = &Package{
			ImportPath: p.PkgPath,
			Name:       p.Name,
			Dir:        p.Dir,
			GoFiles:    p.GoFiles,
			Imports:    importPaths(p),
			ExportFile: p.ExportFile,
			Root:       false,
			ForTest:    p.ForTest,
		}
	}
	return out
}

// importPaths returns the real, deduplicated, sorted import paths of p's
// direct imports — read from each entry's own PkgPath, deliberately not
// from p.Imports' map keys. For an ordinary package the two coincide, but
// for a test-variant p (see fromPackages) a key can itself be another go
// list variant ID (e.g. an external test package "q_test [q.test]"
// importing its base package under the key "p [p.test]", not "p") that
// must never leak into this graph's own Package.Imports as if it were an
// importable path.
func importPaths(p *packages.Package) []string {
	seen := make(map[string]bool, len(p.Imports))
	imports := make([]string, 0, len(p.Imports))
	for _, imp := range p.Imports {
		if seen[imp.PkgPath] {
			continue
		}
		seen[imp.PkgPath] = true
		imports = append(imports, imp.PkgPath)
	}
	sort.Strings(imports)
	return imports
}

// extraImports returns the members of testImports not already present in
// base, preserving testImports' order (already sorted, since it always
// comes from importPaths) — the "extra" dependency set an in-package test
// variant pulls in beyond its base package's own production Imports.
func extraImports(base, testImports []string) []string {
	baseSet := make(map[string]bool, len(base))
	for _, b := range base {
		baseSet[b] = true
	}
	var extra []string
	for _, t := range testImports {
		if !baseSet[t] {
			extra = append(extra, t)
		}
	}
	return extra
}

// isSyntheticTestBinary reports whether p is go list's synthesized "q.test"
// executable for a query pattern matching q (Q1: "q.test" -- q's test
// executable) rather than a real, importable Go package. Nothing can import
// it by path, and — unlike every other node fromPackages materializes —
// its Dir coincides with the real package q's own Dir while its GoFiles
// points at a go-build cache artifact rather than a source file; keeping it
// out of the canonical map avoids exactly the Dir collision that would
// otherwise corrupt internal/check.GraphSource's directory-keyed fallback
// lookup for q itself. Its own dependencies (flag, os/exec, ...) are still
// visited and gain ordinary canonical entries like any other plain package
// — gopls's own metadata graph does not prune those either (Q1) — only
// this one non-importable node is excluded.
func isSyntheticTestBinary(p *packages.Package) bool {
	return p.Name == "main" && strings.HasSuffix(p.PkgPath, ".test")
}

// newSnapshot builds a Snapshot from a package map, computing the topo
// order and reverse-dependency index. dir and buildFlags are remembered so
// ExportFile can re-run packages.Load for a single package if the entry
// already in pkgs turns out to be stale or was never populated.
func newSnapshot(pkgs map[string]*Package, dir string, buildFlags []string) (*Snapshot, error) {
	order, err := topoOrder(pkgs)
	if err != nil {
		return nil, err
	}
	revDeps := make(map[string][]string, len(pkgs))
	for path, pkg := range pkgs {
		for _, imp := range pkg.Imports {
			if _, ok := pkgs[imp]; !ok {
				continue // not part of the loaded graph
			}
			revDeps[imp] = append(revDeps[imp], path)
		}
	}
	for _, importers := range revDeps {
		sort.Strings(importers)
	}
	return &Snapshot{Packages: pkgs, Order: order, dir: dir, buildFlags: buildFlags, revDeps: revDeps}, nil
}

// topoOrder returns the import paths of pkgs in Kahn topological order,
// dependencies before dependents. Import paths outside pkgs (external to
// the loaded graph) are treated as already satisfied. Ties are broken
// lexicographically for a deterministic order.
func topoOrder(pkgs map[string]*Package) ([]string, error) {
	indegree := make(map[string]int, len(pkgs))
	dependents := make(map[string][]string, len(pkgs))
	for path, pkg := range pkgs {
		for _, imp := range pkg.Imports {
			if _, ok := pkgs[imp]; !ok {
				continue
			}
			indegree[path]++
			dependents[imp] = append(dependents[imp], path)
		}
	}

	ready := make([]string, 0, len(pkgs))
	for path := range pkgs {
		if indegree[path] == 0 {
			ready = append(ready, path)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(pkgs))
	for len(ready) > 0 {
		path := ready[0]
		ready = ready[1:]
		order = append(order, path)
		var newlyReady []string
		for _, dep := range dependents[path] {
			indegree[dep]--
			if indegree[dep] == 0 {
				newlyReady = append(newlyReady, dep)
			}
		}
		if len(newlyReady) > 0 {
			ready = append(ready, newlyReady...)
			sort.Strings(ready)
		}
	}

	if len(order) != len(pkgs) {
		return nil, fmt.Errorf("graph: import cycle detected (topo order has %d of %d packages)", len(order), len(pkgs))
	}
	return order, nil
}

// Package returns the graph node for path, if present.
func (s *Snapshot) Package(path string) (*Package, bool) {
	p, ok := s.Packages[path]
	return p, ok
}

// Dir returns the workspace root directory Load ran from (Options.Dir),
// i.e. the base a source file path can be made relative to for storage in a
// root-relative facts database (see internal/index.Options.RelativePaths
// and internal/xref.New).
func (s *Snapshot) Dir() string { return s.dir }

// ExportFile returns the GOCACHE-generated export data file for path, if
// the graph has one. Satisfies internal/typecheck.ExportFileSource.
//
// The path packages.Load reported at Load time can go stale (evicted by
// GOCACHE trimming) or, rarely, never get populated in the first place (a
// transient failure inside the `go list -export` the underlying build
// system runs, unrelated to path's own source). Rather than surface that
// as a permanent "no export data for path" to every caller for the rest of
// this Snapshot's lifetime, ExportFile verifies the file still exists and,
// if not, recovers with one scoped packages.Load for path alone — caching
// the outcome, success or failure, in s.recovered so every later call for
// path (this Snapshot's whole lifetime — see s.recovered's doc for how a
// later Snapshot gets a fresh chance) reuses it instead of paying for
// another recovery subprocess. This matters for a query-time caller in
// particular: ExportFile must never become a place a request handler
// synchronously re-runs go/packages.Load on every call for a package that
// can never be recovered.
func (s *Snapshot) ExportFile(path string) (string, bool) {
	p, ok := s.Packages[path]
	if !ok {
		return "", false
	}
	if p.ExportFile != "" {
		if _, err := os.Stat(p.ExportFile); err == nil {
			return p.ExportFile, true
		}
	}
	if v, ok := s.recovered.Load(path); ok {
		recovered, _ := v.(string)
		if recovered == recoveryFailed {
			return "", false
		}
		if _, err := os.Stat(recovered); err == nil {
			return recovered, true
		}
		s.recovered.Delete(path)
	}
	file, ok := reloadExportFile(s.dir, s.buildFlags, path)
	if ok {
		s.recovered.Store(path, file)
		return file, true
	}
	s.recovered.Store(path, recoveryFailed)
	return "", false
}

// reloadExportFile re-runs packages.Load for the single package pkgPath,
// the recovery path ExportFile falls back to. It requests only
// exportFileMode (no NeedDeps, NeedFiles, ...): pkgPath's dependencies are
// already known from the Snapshot that called it, so this only needs
// pkgPath's own (possibly freshly rebuilt) export data file.
func reloadExportFile(dir string, buildFlags []string, pkgPath string) (string, bool) {
	cfg := &packages.Config{
		Mode:       exportFileMode,
		Dir:        dir,
		BuildFlags: buildFlags,
		Env:        os.Environ(),
	}
	pkgs, err := packages.Load(cfg, pkgPath)
	if err != nil || len(pkgs) != 1 {
		return "", false
	}
	p := pkgs[0]
	if len(p.Errors) > 0 || p.ExportFile == "" {
		return "", false
	}
	if _, err := os.Stat(p.ExportFile); err != nil {
		return "", false
	}
	return p.ExportFile, true
}

// ClosureUnits returns pkgPath plus the import path of every package in the
// graph that (transitively) imports pkgPath — the set of packages whose
// type check result can depend on pkgPath's public API. Mirrors
// gopls-lazy's revIndex.ClosureUnits, applied to the go/packages import
// graph instead of a parsed-imports index.
func (s *Snapshot) ClosureUnits(pkgPath string) []string {
	seen := map[string]bool{pkgPath: true}
	queue := []string{pkgPath}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		for _, importer := range s.revDeps[path] {
			if !seen[importer] {
				seen[importer] = true
				queue = append(queue, importer)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
