package server

import (
	"sort"
	"strings"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/langfeat"
)

// maxUnimportedPackages bounds how many shape-1 (bare package-name prefix)
// unimported candidates a completion response computes an import edit for
// — mirrors gopls's own maxUnimportedPackageNames. Matching workspace.
// pkgNameIndex against the typed prefix is cheap (a map scan), but turning
// each match into a CompletionItem parses, edits, and reformats the whole
// file (see langfeat.importInsertEdit), and a completion list cluttered
// with dozens of "import this and finish typing" entries is not useful
// anyway.
const maxUnimportedPackages = 5

// appendUnimportedCompletions extends items (already-computed in-scope
// completions for cf's cursor) with unimported-package candidates, if any
// apply there — see langfeat.Unimported for how the cursor position
// decides between shape 1 (a bare package-name prefix) and shape 2 (a
// qualified "pkg.Member" selector whose pkg is not yet imported). Returns
// items unchanged if the workspace is not ready or no unimported lookup
// applies at cf's cursor.
func (s *Server) appendUnimportedCompletions(cf checkedFileResult, items []langfeat.CompletionItem) []langfeat.CompletionItem {
	ws := s.workspace()
	if ws == nil {
		return items
	}
	uctx, ok := langfeat.Unimported(cf.cp, cf.text, cf.path, cf.offset)
	if !ok {
		return items
	}
	if uctx.Selector == "" {
		candidates := unimportedPackageCandidates(ws, cf.cp, uctx.Prefix)
		if len(candidates) == 0 {
			return items
		}
		return langfeat.MergeUnimported(items, langfeat.UnimportedPackageItems(cf.path, cf.text, uctx.Prefix, candidates))
	}
	memberItems := s.unimportedMemberItems(ws, cf, uctx)
	if len(memberItems) == 0 {
		return items
	}
	return langfeat.MergeUnimported(items, memberItems)
}

// unimportedPackageCandidates returns, for a shape-1 lookup, one
// UnimportedPackageCandidate per graph-known package whose declared name
// has prefix as a prefix — excluding cp's own package and any package
// already imported into it (both would otherwise duplicate what ordinary
// lexical completion already offers as a resolved *types.PkgName) —
// ordered by name then import path for determinism, capped at
// maxUnimportedPackages.
func unimportedPackageCandidates(ws *workspace, cp *check.CheckedPackage, prefix string) []langfeat.UnimportedPackageCandidate {
	alreadyImported := importedPackagePaths(cp)
	ownPath := cp.PkgPath()

	names := make([]string, 0, len(ws.pkgNameIndex))
	for name := range ws.pkgNameIndex {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var out []langfeat.UnimportedPackageCandidate
	for _, name := range names {
		for _, path := range ws.pkgNameIndex[name] {
			if path == ownPath || alreadyImported[path] {
				continue
			}
			if len(out) >= maxUnimportedPackages {
				return out
			}
			out = append(out, langfeat.UnimportedPackageCandidate{Name: name, ImportPath: path})
		}
	}
	return out
}

// unimportedMemberItems resolves a shape-2 "pkg.Prefix" lookup: it tries
// every graph-known package sharing uctx.Selector's declared name, in
// sorted import-path order (ws.pkgNameIndex is already sorted; ties are
// broken lexically — golance's graph-only v1 candidate source has no
// stdlib/module-cache distinction to prefer between them the way gopls's
// fuller priority list does, a documented simplification), decoding each
// candidate's export data on demand via ws.depCache and stopping at the
// first one whose exported members actually match uctx.Prefix — mirroring
// gopls's own "stop at first success" behavior.
func (s *Server) unimportedMemberItems(ws *workspace, cf checkedFileResult, uctx langfeat.UnimportedContext) []langfeat.CompletionItem {
	paths := ws.pkgNameIndex[uctx.Selector]
	if len(paths) == 0 {
		return nil
	}
	ownPath := cf.cp.PkgPath()
	imp := ws.depCache.importer()
	for _, path := range paths {
		if path == ownPath {
			continue // never suggest importing the package being edited
		}
		pkg, err := imp.ImportFrom(path, "", 0)
		if err != nil {
			continue
		}
		candidate := langfeat.UnimportedPackageCandidate{Name: uctx.Selector, ImportPath: path}
		if items := langfeat.UnimportedMemberItems(cf.path, cf.text, uctx.Prefix, candidate, pkg); len(items) > 0 {
			return items
		}
	}
	return nil
}

// importedPackagePaths returns the import path of every package cp's
// package already imports, so unimportedPackageCandidates can skip
// suggesting one a second time as unimported.
func importedPackagePaths(cp *check.CheckedPackage) map[string]bool {
	imports := cp.Package().Imports()
	out := make(map[string]bool, len(imports))
	for _, imp := range imports {
		out[imp.Path()] = true
	}
	return out
}
