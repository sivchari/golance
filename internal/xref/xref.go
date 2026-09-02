package xref

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/sivchari/golance/internal/index"
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
// searching only the union of the defining package's (and, for a method,
// every corresponding method's own defining package's) reverse-dependency
// closures (see package doc and locationsForAll). includeDecl controls
// whether the declaration itself is included.
//
// When the symbol is a method, the result also includes references to its
// corresponding method on "the other side" of an interface-satisfaction
// relationship (see correspondingMethodSymbols), in both directions:
//
//   - Interface method -> every workspace implementer's matching method
//     (methodImplementationSymbols): a call through a concretely-typed
//     value resolves to the concrete method's own SymbolID, not the
//     interface method's.
//   - Concrete method -> every workspace interface it satisfies that
//     declares a method by this name (interfacesSatisfiedByMethod): a call
//     through an interface-typed value resolves to the interface method's
//     own SymbolID, not the concrete method's.
//
// Either way, exact-SymbolID matching alone would otherwise miss every such
// call site, even though it is exactly the kind of call gopls's own
// References treats as a reference to "the same" method. The concrete ->
// interfaces direction used to be omitted here as too expensive (unioning
// LookupMethod candidates across a concrete type's entire method set), but
// interfacesSatisfiedByMethod bounds candidate gathering to a single
// posting list -- this method's own name -- instead, removing that cost;
// see its doc for the full reasoning. Declarations of those corresponding
// methods are never added (includeDecl only ever controls target's own
// declaration), matching Definition/Rename's existing "one symbol, one
// declaration" behavior.
func (r *Resolver) References(ctx context.Context, file string, line, col int, includeDecl bool) ([]Location, error) {
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		return nil, err
	}
	enterPhase(ctx, "resolve")
	target, err := r.resolveAt(ctx, file, l, c)
	if err != nil {
		return nil, err
	}

	wanted := []resolvedSymbol{target}
	if target.Kind == index.KindMethod {
		corresponding, err := r.correspondingMethodSymbols(ctx, target)
		if err != nil {
			return nil, err
		}
		wanted = append(wanted, corresponding...)
	}

	var out []Location
	if includeDecl {
		_, _, loc, ok := r.symbolByHash(ctx, target.PkgHash, target.IDHash)
		if !ok {
			return nil, fmt.Errorf("xref: definition of %s not found in its own package facts", target.Name)
		}
		out = append(out, loc)
	}

	enterPhase(ctx, "closureWalk")
	refs, err := r.locationsForAll(ctx, wanted)
	if err != nil {
		return nil, err
	}
	out = append(out, refs...)

	enterPhase(ctx, "sortDedup")
	out = dedupeLocations(out)
	sortLocations(out)
	return out, nil
}

// dedupeLocations removes duplicate Locations (comparing every field),
// preserving the first occurrence's position otherwise. References can
// merge several independently-sorted location lists (target's own plus one
// per corresponding method), so a location that -- however unlikely --
// turns up in more than one of them must still be reported only once.
func dedupeLocations(locs []Location) []Location {
	seen := make(map[Location]bool, len(locs))
	out := locs[:0]
	for _, l := range locs {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// wantedSymbolKey identifies one symbol as a locationsForAll scan target,
// independent of its Kind/Name (a unit's own recorded Ref carries only a
// (ToPkgHash, ToSymbolIDHash) pair to match against — see store.Ref).
type wantedSymbolKey struct {
	PkgHash uint64
	IDHash  uint64
}

// locationsForAll collects every location referencing any symbol in wanted,
// across the UNION of their defining packages' reverse-dependency closures,
// visiting each unit in that union exactly once regardless of how many of
// wanted's symbols its own closure would individually include: a reference
// to a symbol can only ever be recorded in a unit whose package
// transitively imports that symbol's defining package (see
// [internal/graph.Snapshot.ClosureUnits]), so checking every wanted symbol
// against a unit outside its own individual closure costs a few no-op map
// lookups, never a false match.
//
// This replaces what used to be one full closure walk per wanted symbol
// (locationsFor, called once for References' own target plus once more per
// corresponding method — see References' doc): a method with K
// corresponding symbols used to re-walk and re-read up to K+1 overlapping
// closures, each its own os.ReadFile plus O(refsCount) store.View.RefsTo
// scan; this does it in one pass, checking every wanted symbol against
// each visited unit's ref records in a single O(refsCount) scan instead of
// K+1 separate O(refsCount) RefsTo calls.
//
// ctx is checked once per unit in the union (rather than per reference
// within a unit): a canceled query stops before starting the next unit's
// facts read instead of running to completion regardless.
func (r *Resolver) locationsForAll(ctx context.Context, wanted []resolvedSymbol) ([]Location, error) {
	wantedSet, units, err := r.wantedClosure(wanted)
	if err != nil {
		return nil, err
	}

	var out []Location
	for p := range units {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		locs, err := r.scanUnitForWanted(ctx, p, wantedSet)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, locs...)
	}

	sortLocations(out)
	return out, nil
}

// wantedClosure builds locationsForAll's two inputs from wanted: the set of
// (PkgHash, IDHash) keys a visited unit's own ref records are checked
// against, and the union of every wanted symbol's defining package's
// reverse-dependency closure (the set of units that union is checked
// against).
func (r *Resolver) wantedClosure(wanted []resolvedSymbol) (map[wantedSymbolKey]struct{}, map[string]struct{}, error) {
	wantedSet := make(map[wantedSymbolKey]struct{}, len(wanted))
	units := make(map[string]struct{})
	for _, w := range wanted {
		wantedSet[wantedSymbolKey{PkgHash: w.PkgHash, IDHash: w.IDHash}] = struct{}{}
		defPkgPath, ok := r.pkgPathByHash[w.PkgHash]
		if !ok {
			return nil, nil, fmt.Errorf("xref: unknown defining package for hash %d", w.PkgHash)
		}
		for _, p := range r.snap.ClosureUnits(defPkgPath) {
			units[p] = struct{}{}
		}
	}
	return wantedSet, units, nil
}

// scanUnitForWanted reads pkgPath's Facts and returns every location whose
// ref record matches a key in wantedSet, scanning the whole ref table in
// one pass (see locationsForAll's doc for why this replaces one
// store.View.RefsTo call per wanted symbol). Reports pkgPath's own read
// size and ref-record count to ctx's installed StatsSink (see addUnit),
// unconditionally -- including when out ends up empty, since the read and
// scan cost was paid either way.
func (r *Resolver) scanUnitForWanted(ctx context.Context, pkgPath string, wantedSet map[wantedSymbolKey]struct{}) ([]Location, error) {
	facts, err := r.unitFacts(ctx, store.Hash(pkgPath))
	if err != nil {
		return nil, err
	}
	v, err := store.NewView(facts)
	if err != nil {
		return nil, err
	}

	n := v.RefsCount()
	var out []Location
	for i := 0; i < n; i++ {
		ref, err := v.RefAt(i)
		if err != nil {
			continue
		}
		key := wantedSymbolKey{PkgHash: ref.ToPkgHash(), IDHash: ref.ToSymbolIDHash()}
		if _, ok := wantedSet[key]; !ok {
			continue
		}
		path, err := v.FileAt(int(ref.FileIdx()))
		if err != nil {
			continue
		}
		out = append(out, Location{File: absPath(r.root, path, r.relative), Line: ref.Line(), Col: ref.Col(), EndCol: ref.EndCol()})
	}
	addUnit(ctx, int64(len(facts)), n)
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

	_, _, declLoc, ok := r.symbolByHash(ctx, target.PkgHash, target.IDHash)
	if !ok {
		return nil, fmt.Errorf("xref: definition of %s not found in its own package facts", target.Name)
	}
	refs, err := r.locationsForAll(ctx, []resolvedSymbol{target})
	if err != nil {
		return nil, err
	}
	locs := append([]Location{declLoc}, refs...)
	sortLocations(locs)

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
