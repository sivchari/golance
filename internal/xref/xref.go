package xref

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/sivchari/golance/internal/store"
)

// toUint32Pos converts line and col — always non-negative in practice, an
// LSP client's own byte-offset-derived position (see
// internal/server.xrefPosition) — to the uint32 coordinates resolveAt
// takes, erroring instead of silently wrapping around if either is
// somehow negative.
func toUint32Pos(line, col int) (uint32, uint32, error) {
	if line < 0 {
		return 0, 0, fmt.Errorf("xref: negative line %d", line)
	}
	if line > math.MaxUint32 {
		return 0, 0, fmt.Errorf("xref: line %d exceeds uint32 range", line)
	}
	if col < 0 {
		return 0, 0, fmt.Errorf("xref: negative col %d", col)
	}
	if col > math.MaxUint32 {
		return 0, 0, fmt.Errorf("xref: col %d exceeds uint32 range", col)
	}
	return uint32(line), uint32(col), nil
}

// Definition returns the declaration location of the symbol at (file, line,
// col). If the cursor is already on the declaration, it resolves to itself.
func (r *Resolver) Definition(ctx context.Context, file string, line, col int) ([]Location, error) {
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		return nil, err
	}
	target, err := r.resolveAt(ctx, file, l, c)
	if err != nil {
		return nil, err
	}
	_, _, loc, ok := r.symbolByHash(ctx, target.PkgHash, target.IDHash)
	if !ok {
		return nil, fmt.Errorf("xref: definition of %s not found in its own package facts", target.Name)
	}
	return []Location{loc}, nil
}

// References returns every reference to the symbol at (file, line, col),
// searching only the defining package plus its reverse-dependency closure
// (see package doc). includeDecl controls whether the declaration itself is
// included.
func (r *Resolver) References(ctx context.Context, file string, line, col int, includeDecl bool) ([]Location, error) {
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		return nil, err
	}
	target, err := r.resolveAt(ctx, file, l, c)
	if err != nil {
		return nil, err
	}
	return r.locationsFor(ctx, target, includeDecl)
}

// locationsFor collects every location referencing target across its
// defining package's reverse-dependency closure. ctx is checked once per
// package in the closure (rather than per reference within a package): a
// canceled query stops before starting the next package's facts read
// instead of running to completion regardless.
func (r *Resolver) locationsFor(ctx context.Context, target resolvedSymbol, includeDecl bool) ([]Location, error) {
	defPkgPath, ok := r.pkgPathByHash[target.PkgHash]
	if !ok {
		return nil, fmt.Errorf("xref: unknown defining package for hash %d", target.PkgHash)
	}

	var out []Location
	if includeDecl {
		_, _, loc, ok := r.symbolByHash(ctx, target.PkgHash, target.IDHash)
		if !ok {
			return nil, fmt.Errorf("xref: definition of %s not found in its own package facts", target.Name)
		}
		out = append(out, loc)
	}

	for _, p := range r.snap.ClosureUnits(defPkgPath) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pkgHash := store.Hash(p)
		u, err := r.unitBlob(ctx, pkgHash)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		v, err := store.NewView(u.Facts)
		if err != nil {
			return nil, err
		}
		for _, ref := range v.RefsTo(target.IDHash) {
			if ref.ToPkgHash() != target.PkgHash {
				continue // idHash collision with an unrelated symbol
			}
			path, err := v.FileAt(int(ref.FileIdx()))
			if err != nil {
				continue
			}
			out = append(out, Location{File: absPath(r.root, path, r.relative), Line: ref.Line(), Col: ref.Col(), EndCol: ref.EndCol()})
		}
	}

	sortLocations(out)
	return out, nil
}

// WorkspaceSymbol returns every symbol whose name starts with query
// (case-insensitive), up to defaultWorkspaceSymbolLimit results. ctx is
// checked once per matched name (rather than per idHash under a name): a
// canceled query stops before resolving the next name's matches instead of
// running to completion regardless.
func (r *Resolver) WorkspaceSymbol(ctx context.Context, query string) ([]SymbolInfo, error) {
	matches, err := r.db.LookupNamePrefix(ctx, query)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(matches))
	for n := range matches {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []SymbolInfo
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, idHash := range matches[n] {
			if len(out) >= defaultWorkspaceSymbolLimit {
				return out, nil
			}
			info, ok, err := r.symbolInfoFromIDHash(ctx, idHash)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, info)
			}
		}
	}
	return out, nil
}

const defaultWorkspaceSymbolLimit = 100

// symbolInfoFromIDHash resolves idHash to a SymbolInfo by recovering its
// defining package from the strings recorded via [store.DB.PutSymbolIDString].
func (r *Resolver) symbolInfoFromIDHash(ctx context.Context, idHash uint64) (SymbolInfo, bool, error) {
	strs, err := r.db.SymbolIDStrings(ctx, idHash)
	if err != nil {
		return SymbolInfo{}, false, err
	}
	for _, s := range strs {
		pkgPath, _, ok := splitSymbolID(s)
		if !ok {
			continue
		}
		name, kind, loc, ok := r.symbolByHash(ctx, store.Hash(pkgPath), idHash)
		if !ok {
			continue
		}
		return SymbolInfo{Name: name, Kind: kind, Container: pkgPath, Location: loc}, true, nil
	}
	return SymbolInfo{}, false, nil
}

// Rename returns the edits needed to rename the symbol at (file, line, col)
// to newName, grouped by file. It includes every reference across the
// defining package's reverse-dependency closure plus the declaration itself.
//
// TODO(v0.1): no collision check against an existing newName in scope.
func (r *Resolver) Rename(ctx context.Context, file string, line, col int, newName string) (map[string][]Edit, error) {
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		return nil, err
	}
	target, err := r.resolveAt(ctx, file, l, c)
	if err != nil {
		return nil, err
	}
	locs, err := r.locationsFor(ctx, target, true)
	if err != nil {
		return nil, err
	}

	edits := make(map[string][]Edit)
	for _, loc := range locs {
		edits[loc.File] = append(edits[loc.File], Edit{Line: loc.Line, Col: loc.Col, EndCol: loc.EndCol, NewText: newName})
	}
	return edits, nil
}

func sortLocations(locs []Location) {
	sort.Slice(locs, func(i, j int) bool {
		a, b := locs[i], locs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
}
