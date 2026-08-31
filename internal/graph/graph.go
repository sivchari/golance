// Package graph loads the workspace's Go import graph via go/packages and
// keeps it available as a Snapshot: package metadata, a topological build
// order, and reverse-dependency closures. It deliberately never requests
// type or syntax information (NeedTypes/NeedSyntax) — that is internal/typecheck's
// job, done on demand per package.
package graph

import (
	"fmt"
	"os"
	"sort"
	"sync"

	"golang.org/x/tools/go/packages"
)

// exportFileMode is the minimal go/packages.Load mode reloadExportFile uses
// to recover a single package's export data. See Snapshot.ExportFile's doc
// for when this runs.
const exportFileMode = packages.NeedName | packages.NeedExportFile

// loadMode is the only go/packages.Load mode graph ever requests. NeedTypes
// and NeedSyntax must never be added here: loading type/syntax information
// for the whole workspace is exactly the cost this package exists to avoid.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedImports |
	packages.NeedDeps | packages.NeedExportFile

// Package is one node in the import graph: a single Go package as reported
// by go/packages, without any type or syntax information.
type Package struct {
	ImportPath string   `json:"importPath"`
	Dir        string   `json:"dir"`
	GoFiles    []string `json:"goFiles"`
	Imports    []string `json:"imports"` // import paths of direct dependencies present in the graph
	ExportFile string   `json:"exportFile,omitempty"`
	Root       bool     `json:"root,omitempty"` // matched a Load pattern directly, as opposed to being pulled in only as a dependency
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

	// recovered caches, per import path, an export file path ExportFile
	// has already recovered via reloadExportFile, so a later call for the
	// same package after the same stat failure reuses it instead of paying
	// for another recovery subprocess. A Snapshot is published once (via
	// Load or LoadCache) and read concurrently by many request handlers
	// from then on (see internal/server's atomic.Pointer[workspace]), so
	// this cache — the one place ExportFile writes anything back — must be
	// synchronized itself rather than a plain map; the Packages field
	// above stays exactly as published, never mutated in place.
	recovered sync.Map // string (import path) -> string (recovered export file path)
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
	}
	if opts.Offline {
		cfg.Env = append(cfg.Env, "GOPROXY=off")
	}
	roots, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("graph: load packages: %w", err)
	}

	rootSet := make(map[string]bool, len(roots))
	for _, p := range roots {
		rootSet[p.PkgPath] = true
	}

	var all []*packages.Package
	packages.Visit(roots, func(p *packages.Package) bool {
		all = append(all, p)
		return true
	}, nil)

	return newSnapshot(fromPackages(all, rootSet), opts.Dir, opts.BuildFlags)
}

// fromPackages converts go/packages results into the graph's own
// lightweight, JSON-serializable Package representation. rootSet marks the
// import paths that matched a Load pattern directly.
func fromPackages(pkgs []*packages.Package, rootSet map[string]bool) map[string]*Package {
	out := make(map[string]*Package, len(pkgs))
	for _, p := range pkgs {
		imports := make([]string, 0, len(p.Imports))
		for path := range p.Imports {
			imports = append(imports, path)
		}
		sort.Strings(imports)
		out[p.PkgPath] = &Package{
			ImportPath: p.PkgPath,
			Dir:        p.Dir,
			GoFiles:    p.GoFiles,
			Imports:    imports,
			ExportFile: p.ExportFile,
			Root:       rootSet[p.PkgPath],
		}
	}
	return out
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
// the recovered path in s.recovered so every later call for path reuses it
// instead of paying for another recovery subprocess.
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
		recovered := v.(string)
		if _, err := os.Stat(recovered); err == nil {
			return recovered, true
		}
		s.recovered.Delete(path)
	}
	file, ok := reloadExportFile(s.dir, s.buildFlags, path)
	if ok {
		s.recovered.Store(path, file)
	}
	return file, ok
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
