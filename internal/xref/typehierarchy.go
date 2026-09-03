package xref

import (
	"context"
	"fmt"
	"go/types"
	"sort"

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
)

// TypeHierarchyItemInfo describes one candidate in a type hierarchy
// Supertypes/Subtypes result: its own name, defining package, whether it is
// an interface, and its declaration location.
type TypeHierarchyItemInfo struct {
	Name        string
	PkgPath     string
	IsInterface bool
	Location    Location
}

// Supertypes returns every interface in the workspace that the named type
// or interface at (file, line, col) implements: golance's counterpart of
// gopls's TypeHierarchy Supertypes relation
// (golang.org/x/tools/gopls/internal/golang/type_hierarchy.go's
// relatedTypes, called with methodsets.Supertype). A concrete type's
// supertypes are the interfaces it satisfies -- the identical relation
// Implementation's own concrete -> interfaces direction already computes
// (see interfacesImplementedBy) -- while an interface's supertypes are the
// OTHER interfaces whose method set it is a structural superset of:
// typically, but not exclusively (this is a method-set comparison, not a
// literal AST embeds-scan, matching gopls's own methodsets.Index.Search),
// the interfaces it embeds.
func (r *Resolver) Supertypes(ctx context.Context, file string, line, col int) ([]TypeHierarchyItemInfo, error) {
	named, key, err := r.typeHierarchyTarget(ctx, file, line, col)
	if err != nil {
		return nil, err
	}

	queryType := methodSetType(named)
	ms := types.NewMethodSet(queryType)
	if ms.Len() == 0 {
		// A type with no methods trivially implements every zero-method
		// interface (chiefly interface{}/any); no point reporting that,
		// mirroring interfacesImplementedBy's identical guard.
		return nil, nil
	}
	names := make([]string, ms.Len())
	for i := 0; i < ms.Len(); i++ {
		names[i] = ms.At(i).Obj().Name()
	}

	diag := newImplDiag(names)
	candidates, err := r.candidatesByAnyMethod(ctx, names, index.KindInterface, diag)
	if err != nil {
		return nil, err
	}

	var out []TypeHierarchyItemInfo
	for k := range candidates {
		if k == key {
			continue // never report the queried type as its own supertype
		}
		info, ok, err := r.confirmSupertypeCandidate(ctx, k, queryType, diag)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, info)
		}
	}
	sortTypeHierarchyItemInfos(out)
	if len(out) == 0 {
		r.logImplDiag("type hierarchy supertypes of "+named.Obj().Name(), diag)
	}
	return out, nil
}

// confirmSupertypeCandidate resolves and confirms one Supertypes candidate
// k: k qualifies once its decoded *types.Interface is genuinely implemented
// by queryType, mirroring interfacesImplementedBy's identical decode-based
// confirmation (no fingerprint fast path on this side -- see
// implementedInterfaces' own doc for why that asymmetry is intentional).
func (r *Resolver) confirmSupertypeCandidate(ctx context.Context, k candidateKey, queryType types.Type, diag *implDiag) (TypeHierarchyItemInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return TypeHierarchyItemInfo{}, false, err
	}
	iname, _, loc, ok := r.symbolByHash(ctx, k.PkgHash, k.TypeSymbolIDHash)
	if !ok {
		return TypeHierarchyItemInfo{}, false, nil
	}
	ipath, ok := r.pkgPathByHash[k.PkgHash]
	if !ok {
		return TypeHierarchyItemInfo{}, false, nil
	}
	inamed, err := r.resolveNamed(ctx, ipath, iname)
	if err != nil {
		diag.skip(ipath, iname, err)
		return TypeHierarchyItemInfo{}, false, nil
	}
	iface, ok := inamed.Underlying().(*types.Interface)
	if !ok {
		return TypeHierarchyItemInfo{}, false, nil
	}
	if !types.Implements(queryType, iface) {
		return TypeHierarchyItemInfo{}, false, nil
	}
	diag.survivors++
	return TypeHierarchyItemInfo{Name: iname, PkgPath: ipath, IsInterface: true, Location: loc}, true, nil
}

// Subtypes returns every type in the workspace -- concrete or interface --
// that implements the interface at (file, line, col): golance's counterpart
// of gopls's TypeHierarchy Subtypes relation (relatedTypes called with
// methodsets.Subtype). Per gopls's own method-set-based Subtype relation,
// which requires the query type itself to be an interface (see implements'
// "if !types.IsInterface(y) { return false }" in gopls's implementation.go),
// a concrete query type's subtypes are always empty: Go has no notion of
// subclassing a struct. Unlike Implementation's own interface ->
// implementers direction (implementationsOfInterface, restricted to
// index.KindType so "Go to Implementations" reports only concrete
// implementers), this deliberately also includes every OTHER interface
// satisfying the query interface (typically by embedding it), matching
// gopls's identical index-wide, kind-unaware Subtype search
// (methodsets.Index.Search scans every package-level type/interface's
// method set alike, with no kind filter of its own).
func (r *Resolver) Subtypes(ctx context.Context, file string, line, col int) ([]TypeHierarchyItemInfo, error) {
	named, key, err := r.typeHierarchyTarget(ctx, file, line, col)
	if err != nil {
		return nil, err
	}
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, nil // concrete query type: no subtypes, matching gopls
	}
	if iface.NumMethods() == 0 {
		// interface{}/any: every type in the workspace trivially qualifies,
		// not a useful result -- mirrors implementationsOfInterface's
		// identical guard for "Go to Implementations".
		return nil, nil
	}
	names := make([]string, iface.NumMethods())
	for i := range names {
		names[i] = iface.Method(i).Name()
	}
	generic := named.TypeParams().Len() > 0
	ifaceFPs := interfaceFingerprints(iface, generic)

	diag := newImplDiag(names)
	candidates, err := r.candidatesByAllMethodsEitherKind(ctx, names, diag)
	if err != nil {
		return nil, err
	}

	var out []TypeHierarchyItemInfo
	for k, byName := range candidates {
		if k == key {
			continue // never report the queried interface as its own subtype
		}
		info, ok, err := r.confirmSubtypeCandidate(ctx, k, byName, iface, names, ifaceFPs, generic, diag)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, info)
		}
	}
	sortTypeHierarchyItemInfos(out)
	if len(out) == 0 {
		r.logImplDiag("type hierarchy subtypes of "+named.Obj().Name(), diag)
	}
	return out, nil
}

// interfaceFingerprints returns iface's own MethodFingerprint per directly
// declared method name, or an empty map when generic is set: a still-generic
// (uninstantiated) receiver's method signature is not canonically comparable
// this way -- see registerMethodSet's identical exclusion in
// internal/index/facts.go.
func interfaceFingerprints(iface *types.Interface, generic bool) map[string]uint64 {
	fps := make(map[string]uint64, iface.NumMethods())
	if generic {
		return fps
	}
	for i := 0; i < iface.NumMethods(); i++ {
		fn := iface.Method(i)
		if sig, ok := fn.Type().(*types.Signature); ok {
			fps[fn.Name()] = index.MethodFingerprint(sig)
		}
	}
	return fps
}

// confirmSubtypeCandidate resolves and confirms one Subtypes candidate k:
// via the fingerprint fast path first (fingerprintsConfirm, the same
// index-only confirmation implementingTypes uses for "Go to
// Implementations" -- see its own doc for the soundness argument and why an
// unexported candidate is resolvable this way), falling back to a live
// types.Implements decode when generic or the fingerprints do not confirm.
func (r *Resolver) confirmSubtypeCandidate(ctx context.Context, k candidateKey, byName map[string][]store.MethodEntry, iface *types.Interface, names []string, ifaceFPs map[string]uint64, generic bool, diag *implDiag) (TypeHierarchyItemInfo, bool, error) {
	cname, ckind, loc, ok := r.symbolByHash(ctx, k.PkgHash, k.TypeSymbolIDHash)
	if !ok {
		return TypeHierarchyItemInfo{}, false, nil
	}
	isInterface := ckind == index.KindInterface
	cpath, ok := r.pkgPathByHash[k.PkgHash]
	if !ok {
		return TypeHierarchyItemInfo{}, false, nil
	}
	if !generic && fingerprintsConfirm(byName, names, ifaceFPs) {
		diag.survivors++
		return TypeHierarchyItemInfo{Name: cname, PkgPath: cpath, IsInterface: isInterface, Location: loc}, true, nil
	}
	if err := ctx.Err(); err != nil {
		return TypeHierarchyItemInfo{}, false, err
	}
	cnamed, err := r.resolveNamed(ctx, cpath, cname)
	if err != nil {
		diag.skip(cpath, cname, err)
		return TypeHierarchyItemInfo{}, false, nil
	}
	if !types.Implements(methodSetType(cnamed), iface) {
		diag.fingerprintMismatch++
		return TypeHierarchyItemInfo{}, false, nil
	}
	diag.survivors++
	return TypeHierarchyItemInfo{Name: cname, PkgPath: cpath, IsInterface: isInterface, Location: loc}, true, nil
}

// typeHierarchyTarget resolves the type/interface at (file, line, col) --
// shared by Supertypes/Subtypes -- returning its *types.Named plus its own
// candidateKey, so the caller can exclude the query type from its own
// result set (a concern implementingTypes/implementedInterfaces never have,
// since each of those searches a kind bucket disjoint from the query's
// own).
func (r *Resolver) typeHierarchyTarget(ctx context.Context, file string, line, col int) (*types.Named, candidateKey, error) {
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		return nil, candidateKey{}, err
	}
	target, err := r.resolveAt(ctx, file, l, c)
	if err != nil {
		return nil, candidateKey{}, err
	}
	if target.Kind != index.KindType && target.Kind != index.KindInterface {
		return nil, candidateKey{}, fmt.Errorf("xref: type hierarchy query not supported for symbol kind %d", target.Kind)
	}
	pkgPath, ok := r.pkgPathByHash[target.PkgHash]
	if !ok {
		return nil, candidateKey{}, fmt.Errorf("xref: unknown defining package for hash %d", target.PkgHash)
	}
	named, err := r.resolveNamed(ctx, pkgPath, target.Name)
	if err != nil {
		return nil, candidateKey{}, err
	}
	return named, candidateKey{PkgHash: target.PkgHash, TypeSymbolIDHash: target.IDHash}, nil
}

// methodSetType returns the type to compute a method set over, or pass as
// the "x" argument to types.Implements, for named: itself for an interface
// (an interface's methods ARE its own declared set; wrapping it in a
// types.Pointer would instead yield an EMPTY method set, since a
// pointer-to-interface has no methods of its own in real Go), or *named for
// a concrete type (whose pointer method set is a superset of its value
// method set) -- the same rule gopls's own methodsets.EnsurePointer applies.
func methodSetType(named *types.Named) types.Type {
	if types.IsInterface(named) {
		return named
	}
	return types.NewPointer(named)
}

// candidatesByAllMethodsEitherKind is candidatesByAllMethods' Subtypes
// counterpart: every candidate of EITHER index.KindType or
// index.KindInterface that has ALL of methodNames recorded, unlike
// candidatesByAllMethods (used only for Implementation's "Go to
// Implementations", which reports concrete implementers alone and so is
// always called with a single wantKind). gopls's own global type-hierarchy
// search is kind-unaware in exactly this way -- see Subtypes' own doc.
func (r *Resolver) candidatesByAllMethodsEitherKind(ctx context.Context, methodNames []string, diag *implDiag) (candidateOccurrences, error) {
	result := make(candidateOccurrences)
	for i, name := range methodNames {
		set, err := r.methodEntriesOfEitherKind(ctx, name, diag)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			for k, entries := range set {
				result[k] = map[string][]store.MethodEntry{name: entries}
			}
			continue
		}
		for k := range result {
			entries, ok := set[k]
			if !ok {
				delete(result, k)
				continue
			}
			result[k][name] = entries
		}
		if len(result) == 0 {
			return nil, nil
		}
	}
	return result, nil
}

// methodEntriesOfEitherKind is methodEntriesOfKind's kind-unaware
// counterpart: every store.MethodEntry [store.DB.LookupMethod] records for
// methodName whose receiver/interface type is EITHER index.KindType or
// index.KindInterface. See methodEntriesOfKind's own doc for why both the
// candidate type and the method itself are re-checked against the current
// facts index here too.
func (r *Resolver) methodEntriesOfEitherKind(ctx context.Context, methodName string, diag *implDiag) (map[candidateKey][]store.MethodEntry, error) {
	entries, err := r.db.LookupMethod(ctx, methodName)
	if err != nil {
		return nil, err
	}
	diag.recordLookup(methodName, len(entries))
	set := make(map[candidateKey][]store.MethodEntry, len(entries))
	for _, e := range entries {
		_, kind, _, ok := r.symbolByHash(ctx, e.PkgHash, e.TypeSymbolIDHash)
		if !ok || (kind != index.KindType && kind != index.KindInterface) {
			continue
		}
		_, mKind, _, mOk := r.symbolByHash(ctx, e.MethodPkgHash, e.MethodIDHash)
		if !mOk || mKind != index.KindMethod {
			continue
		}
		k := candidateKey{PkgHash: e.PkgHash, TypeSymbolIDHash: e.TypeSymbolIDHash}
		set[k] = append(set[k], e)
	}
	return set, nil
}

// sortTypeHierarchyItemInfos sorts items by (package, name, file, line) for
// a deterministic Supertypes/Subtypes result order, independent of Go's
// randomized map iteration.
func sortTypeHierarchyItemInfos(items []TypeHierarchyItemInfo) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.PkgPath != b.PkgPath {
			return a.PkgPath < b.PkgPath
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Location.File != b.Location.File {
			return a.Location.File < b.Location.File
		}
		return a.Location.Line < b.Location.Line
	})
}
