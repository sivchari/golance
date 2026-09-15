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
// Dir/GoFiles, not just the packages production code reaches. Without this,
// internal/depexport's dependency-export importer has nothing to resolve
// "testing" from, and every feature touching a _test.go file's use of it
// (hover, definition, diagnostics) fails outright.
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
	"hash/fnv"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// loadMode is the main go/packages.Load mode graph requests for the whole
// workspace. NeedTypes and NeedSyntax must never be added here: loading
// type/syntax information for the whole workspace is exactly the cost this
// package exists to avoid. NeedForTest is required for fromPackages to tell
// a test variant apart from an ordinary package at all: go/packages leaves
// Package.ForTest permanently "" — even with Config.Tests: true and even
// though the underlying `go list -json` output does carry it — unless this
// bit is requested (see golist.go's addFields, gated on cfg.Mode&NeedForTest).
//
// NeedExportFile is deliberately never requested at all, by this mode or
// any other go/packages.Load call anywhere in golance (see
// internal/depexport's package doc for what replaced it): requesting it
// makes the underlying `go list` run with `-export=true`, which COMPILES
// the matched package's export data with the Go toolchain's own compiler
// rather than just resolving metadata — for a large dependency closure on a
// cold GOCACHE, that meant dozens of full-size `compile` processes running
// at once (the field report internal/depexport exists to fix), all for
// data golance now produces itself via declaration-only source-checking
// (internal/depcheck), the same way gopls resolves a dependency's types.
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
	ForTest string `json:"forTest,omitempty"`
	Root    bool   `json:"root,omitempty"` // matched a Load pattern directly, as opposed to being pulled in only as a dependency
}

// Snapshot is an immutable view of the import graph: every loaded package,
// a topological build order (dependencies before dependents), and reverse
// dependency closures.
type Snapshot struct {
	Packages map[string]*Package `json:"packages"`
	Order    []string            `json:"order"`

	dir string // working directory Load ran from, for Dir()/RelativePaths
	// revDeps maps an import path to every package that directly imports
	// it, present in the graph — folding in both Package.Imports and
	// Package.TestImports (see newSnapshot), so ClosureUnits also reaches a
	// package whose only edge to pkgPath is through an in-package test
	// file.
	revDeps map[string][]string

	// pos maps an import path to its index in Order, for Before — computed
	// once here rather than by every caller that used to derive its own
	// copy from Order (see Before's own doc for why a single shared
	// definition of "positioned before" matters).
	pos map[string]int

	// buildFlagsFP is set by Load from the build configuration
	// packages.Load actually resolved this Snapshot's packages under (see
	// buildFlagsFingerprint and BuildFlagsFingerprint).
	buildFlagsFP string
}

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
	snap := newSnapshot(pkgs, opts.Dir)
	snap.buildFlagsFP = buildFlagsFingerprint(opts.BuildFlags, cfg.Env)
	return snap, nil
}

// buildFlagsFingerprint returns a deterministic fingerprint of the build
// configuration packages.Load actually resolved packages under: buildFlags
// (forwarded to packages.Config.BuildFlags, e.g. -tags) plus GOFLAGS,
// GOOS, GOARCH, and CGO_ENABLED as found in env — the same env Load itself
// passed to go/packages. A caller that folds this into internal/index's
// cache key invalidates the index whenever any of these change, even for a
// package whose own file set happens not to (see
// internal/index.Options.BuildFlagsFingerprint's doc). GOOS/GOARCH/
// CGO_ENABLED left unset in env fold in as empty strings rather than their
// resolved defaults — imprecise but safe: a transition from unset to
// explicitly set to the same effective default value is reported as a
// change, at worst costing one redundant rebuild, never masking a real one.
func buildFlagsFingerprint(buildFlags, env []string) string {
	h := fnv.New64a()
	for _, f := range buildFlags {
		_, _ = h.Write([]byte(f))
		_, _ = h.Write([]byte{0})
	}
	for _, key := range []string{"GOFLAGS", "GOOS", "GOARCH", "CGO_ENABLED"} {
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{'='})
		_, _ = h.Write([]byte(envLookup(env, key)))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// envLookup returns key's value in env (an os.Environ-style "KEY=VALUE"
// slice), or "" if key is not present.
func envLookup(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
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
		// Dir/GoFiles do not vary by context; only its build's internal
		// type identity would, which this plain, non-ITV-aware graph does
		// not model (see the ITV note above).
		if _, exists := out[p.PkgPath]; exists {
			continue
		}
		out[p.PkgPath] = &Package{
			ImportPath: p.PkgPath,
			Name:       p.Name,
			Dir:        p.Dir,
			GoFiles:    p.GoFiles,
			Imports:    importPaths(p),
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
// order and reverse-dependency index. dir is remembered for Dir()'s own use
// (relative-path storage in a root-relative facts database).
func newSnapshot(pkgs map[string]*Package, dir string) *Snapshot {
	order := topoOrder(pkgs)
	revDeps := make(map[string][]string, len(pkgs))
	addRevDeps := func(path string, imports []string) {
		for _, imp := range imports {
			if _, ok := pkgs[imp]; !ok {
				continue // not part of the loaded graph
			}
			revDeps[imp] = append(revDeps[imp], path)
		}
	}
	for path, pkg := range pkgs {
		addRevDeps(path, pkg.Imports)
		// TestImports (see its own doc) is disjoint from Imports by
		// construction (extraImports only ever records what Imports does
		// not already have), so this never adds path to the same
		// revDeps[imp] slice twice for one import path.
		addRevDeps(path, pkg.TestImports)
	}
	for _, importers := range revDeps {
		sort.Strings(importers)
	}
	pos := make(map[string]int, len(order))
	for i, p := range order {
		pos[p] = i
	}
	return &Snapshot{Packages: pkgs, Order: order, dir: dir, revDeps: revDeps, pos: pos}
}

// topoOrder returns the import paths of pkgs in Kahn topological order,
// dependencies before dependents, respecting both Package.Imports and
// Package.TestImports edges — internal/index's directDepExports folds both
// into a package's own combined key, so a package reachable from another
// only through the latter still needs to be processed first. Ties are
// broken lexicographically for a deterministic order.
//
// Imports alone is always guaranteed acyclic — go/packages.Load itself
// rejects a real production import cycle before this ever runs — but
// Imports plus TestImports together are not: an in-package test file may
// legally import a package that, in production, imports the package under
// test (see Package.TestImports's own doc for why that edge is never
// folded into Imports for OTHER cycle-sensitive purposes); go/packages'
// own "p [p.test]"/"p" split treats that as two distinct compilation units,
// so it is not a real build-graph cycle even though it looks like one once
// TestImports edges are added to a single graph here. When that leaves some
// packages permanently unready, the primary kahnPass below stalls before
// covering every package; rather than fail the whole graph load over a
// legal Go pattern, every package still unplaced at that point is instead
// re-ordered by a second kahnPass restricted to Imports alone (guaranteed
// to place every one of them, by the same acyclic guarantee, since a
// subgraph of a DAG induced on any node subset is itself always a DAG) —
// this respects every ordinary production edge among the unplaced set too
// (e.g. a package that merely depends on one half of a test-only cycle
// still lands after it), leaving only the cycle-closing TestImports edge(s)
// themselves genuinely unordered between their own two endpoints, tie-
// broken lexicographically like any other. This makes topoOrder unable to
// fail today, unlike before TestImports edges were added to its graph.
func topoOrder(pkgs map[string]*Package) []string {
	all := make(map[string]bool, len(pkgs))
	for path := range pkgs {
		all[path] = true
	}
	order, placed := kahnPass(all, func(path string) []string {
		pkg := pkgs[path]
		return append(append([]string(nil), pkg.Imports...), pkg.TestImports...)
	})
	if len(order) == len(pkgs) {
		return order
	}

	remaining := make(map[string]bool, len(pkgs)-len(order))
	for path := range pkgs {
		if !placed[path] {
			remaining[path] = true
		}
	}
	rest, _ := kahnPass(remaining, func(path string) []string {
		return pkgs[path].Imports
	})
	return append(order, rest...)
}

// kahnPass runs one Kahn's-algorithm topological sort over scope's members,
// considering only an edgesOf(path) target also present in scope (any
// other target — outside scope entirely, or filtered out by the caller's
// own edgesOf — is treated as already satisfied). Ties among
// simultaneously-ready nodes are broken lexicographically for a
// deterministic order. order holds every member kahnPass managed to place,
// in dependency order; placed reports exactly which those were, for a
// caller whose own edge set might not be acyclic (topoOrder's primary pass)
// to identify what is left over. A caller whose edgesOf is known acyclic
// over scope (topoOrder's second, Imports-only pass) always gets
// len(order) == len(scope).
func kahnPass(scope map[string]bool, edgesOf func(path string) []string) (order []string, placed map[string]bool) {
	indegree := make(map[string]int, len(scope))
	dependents := make(map[string][]string, len(scope))
	for path := range scope {
		for _, dep := range edgesOf(path) {
			if !scope[dep] {
				continue
			}
			indegree[path]++
			dependents[dep] = append(dependents[dep], path)
		}
	}

	ready := make([]string, 0, len(scope))
	for path := range scope {
		if indegree[path] == 0 {
			ready = append(ready, path)
		}
	}
	sort.Strings(ready)

	placed = make(map[string]bool, len(scope))
	order = make([]string, 0, len(scope))
	for len(ready) > 0 {
		path := ready[0]
		ready = ready[1:]
		order = append(order, path)
		placed[path] = true
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
	return order, placed
}

// Before reports whether a is guaranteed to finish processing strictly
// before b in the dependency-ordered walk both Build's scheduler
// (schedulableDepsOf) and Reindex's closure walk (orderedReverseClosure)
// derive from Order — the single shared definition of "safely
// processed-before" every caller that needs to know whether it may rely on
// b's result while processing a should use, instead of each deriving its
// own copy of Order's position map (as internal/index's scheduler and
// directDepImports both used to, independently, before this existed).
//
// False whenever that guarantee does not hold: either path absent from
// Order (e.g. not part of this Snapshot at all), or a >= b's own position —
// which includes every pair topoOrder's own rare test-only-cycle fallback
// could not fully order (see its doc): within that fallback's fixed,
// lexicographic tail, relative position no longer reflects a real
// dependency edge, so treating it as "before" would be exactly the false
// promise that made a dependent ask for a not-yet-processed dependency's
// key ("processed out of order?" — see internal/index/unit.go's
// directDepExports and its own doc for this method's motivating bug).
func (s *Snapshot) Before(a, b string) bool {
	pa, ok := s.pos[a]
	if !ok {
		return false
	}
	pb, ok := s.pos[b]
	if !ok {
		return false
	}
	return pa < pb
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

// BuildFlagsFingerprint returns a fingerprint of the build configuration
// (build flags plus GOFLAGS/GOOS/GOARCH/CGO_ENABLED) Load actually resolved
// s's packages under (see buildFlagsFingerprint).
func (s *Snapshot) BuildFlagsFingerprint() string { return s.buildFlagsFP }

// ClosureUnits returns pkgPath plus the import path of every package in the
// graph that (transitively) imports pkgPath — the set of packages whose
// type check result can depend on pkgPath's public API — via either a
// production import (Package.Imports) or an in-package test file's own
// import (Package.TestImports): a package reached only through the latter
// still needs reprocessing when pkgPath's export data changes, exactly like
// an ordinary importer (see newSnapshot's revDeps construction). Mirrors
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
