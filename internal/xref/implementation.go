package xref

import (
	"context"
	"fmt"
	"go/types"

	"golang.org/x/tools/go/types/objectpath"

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// Implementation returns, for an interface at (file, line, col), every
// concrete type in the workspace that implements it; for a concrete named
// type, every interface in the workspace it implements; for a method name
// (either an interface method's declaration or a concrete type's method),
// the corresponding method of every type on the other side of that same
// relationship (see implementationOfMethod). All directions use a sound
// name-based first pass over the method index followed by a
// types.Implements confirmation against export data (see package doc).
func (r *Resolver) Implementation(ctx context.Context, file string, line, col int) ([]Location, error) {
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		return nil, err
	}
	target, err := r.resolveAt(ctx, file, l, c)
	if err != nil {
		return nil, err
	}
	pkgPath, ok := r.pkgPathByHash[target.PkgHash]
	if !ok {
		return nil, fmt.Errorf("xref: unknown defining package for hash %d", target.PkgHash)
	}

	if target.Kind == index.KindMethod {
		return r.implementationOfMethod(ctx, pkgPath, target)
	}

	named, err := r.resolveNamed(ctx, pkgPath, target.Name)
	if err != nil {
		return nil, err
	}

	switch target.Kind {
	case index.KindInterface:
		return r.implementationsOfInterface(ctx, named)
	case index.KindType:
		return r.interfacesImplementedBy(ctx, named)
	default:
		return nil, fmt.Errorf("xref: implementation query not supported for symbol kind %d", target.Kind)
	}
}

// implementationOfMethod handles a Kind == index.KindMethod target. Unlike
// an interface or concrete type name, a method name is never in its
// package's scope, so resolveNamed cannot look it up -- that gap used to
// make an implementation query on a method name fail outright (see
// resolveMethodFunc for the lookup that replaces it). It dispatches on the
// method's receiver: an interface method's implementers are the matching
// methods of every concrete type implementationsOfInterface would find for
// its interface; a concrete method's implemented interfaces are the
// matching methods of every interface interfacesImplementedBy would find
// for its type.
func (r *Resolver) implementationOfMethod(ctx context.Context, pkgPath string, target resolvedSymbol) ([]Location, error) {
	fn, err := r.resolveMethodFunc(ctx, pkgPath, target.IDHash)
	if err != nil {
		return nil, err
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil, fmt.Errorf("xref: %s has no receiver", target.Name)
	}
	recvType := sig.Recv().Type()
	if ptr, ok := recvType.(*types.Pointer); ok {
		recvType = ptr.Elem()
	}
	named, ok := recvType.(*types.Named)
	if !ok {
		return nil, fmt.Errorf("xref: receiver of %s is not a named type", target.Name)
	}

	if types.IsInterface(named) {
		return r.methodImplementations(ctx, named, target.Name)
	}
	return r.methodInterfaces(ctx, named, target.Name)
}

// resolveMethodFunc decodes pkgPath's export data and looks up the
// *types.Func for the method whose facts-recorded SymbolID hash is idHash,
// via the objectpath it was originally indexed under (see index's
// symbolID) -- the inverse of what facts extraction computed. objectpath
// encodes a method by its structural position within its declaring type
// (not its source position), so this resolves to the same object
// regardless of which independently-decoded copy of pkgPath it comes from.
func (r *Resolver) resolveMethodFunc(ctx context.Context, pkgPath string, idHash uint64) (*types.Func, error) {
	strs, err := r.db.SymbolIDStrings(ctx, idHash)
	if err != nil {
		return nil, err
	}
	var objPath string
	for _, s := range strs {
		p, op, ok := splitSymbolID(s)
		if ok && p == pkgPath {
			objPath = op
			break
		}
	}
	if objPath == "" {
		return nil, fmt.Errorf("xref: no recorded SymbolID for method in package %s", pkgPath)
	}

	u, err := r.unitBlob(ctx, store.Hash(pkgPath))
	if err != nil {
		return nil, fmt.Errorf("xref: read export data for %s: %w", pkgPath, err)
	}
	tpkg, err := typecheck.ReadExport(u.Export, r.fset, pkgPath, r.cache)
	if err != nil {
		return nil, fmt.Errorf("xref: decode export data for %s: %w", pkgPath, err)
	}
	obj, err := objectpath.Object(tpkg, objectpath.Path(objPath))
	if err != nil {
		return nil, fmt.Errorf("xref: resolve %s#%s: %w", pkgPath, objPath, err)
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, fmt.Errorf("xref: %s#%s is not a method", pkgPath, objPath)
	}
	return fn, nil
}

// implementationsOfInterface finds every concrete type in the workspace
// that implements the interface named.
//
// The empty interface (interface{}/any) is deliberately excluded: every
// type in the workspace trivially implements it, so "every type" is not a
// useful "Go to Implementations" result — the same call gopls makes for
// the same reason. Return no results rather than enumerating the
// workspace.
func (r *Resolver) implementationsOfInterface(ctx context.Context, named *types.Named) ([]Location, error) {
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("xref: %s is not an interface", named.Obj().Name())
	}
	if iface.NumMethods() == 0 {
		return nil, nil
	}
	names := make([]string, iface.NumMethods())
	for i := range names {
		names[i] = iface.Method(i).Name()
	}

	impls, err := r.implementingTypes(ctx, iface, names)
	if err != nil {
		return nil, err
	}
	var out []Location
	for entry := range impls {
		_, _, loc, ok := r.symbolByHash(ctx, entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	return out, nil
}

// methodImplementations is implementationsOfInterface's method-granular
// counterpart, for an implementation query on iface's methodName method
// itself rather than on iface's name: it returns each implementer's
// matching method location instead of the implementer type's own
// declaration location.
func (r *Resolver) methodImplementations(ctx context.Context, iface *types.Named, methodName string) ([]Location, error) {
	ifaceType, ok := iface.Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("xref: %s is not an interface", iface.Obj().Name())
	}
	names := make([]string, ifaceType.NumMethods())
	for i := range names {
		names[i] = ifaceType.Method(i).Name()
	}

	impls, err := r.implementingTypes(ctx, ifaceType, names)
	if err != nil {
		return nil, err
	}
	var out []Location
	for entry, cnamed := range impls {
		loc, ok := r.concreteMethodLocation(ctx, entry.PkgHash, cnamed, methodName)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	return out, nil
}

// implementingTypes intersects [store.DB.LookupMethod] candidates across
// every one of methodNames (a real implementer must have all of them, so a
// candidate missing even one is never decoded), then confirms each
// survivor with types.Implements, returning the ones that pass alongside
// their resolved *types.Named. ctx is checked once per candidate: a
// canceled query stops before decoding the next candidate's export data
// (the expensive part of this loop) instead of running to completion
// regardless.
func (r *Resolver) implementingTypes(ctx context.Context, iface *types.Interface, methodNames []string) (map[store.MethodEntry]*types.Named, error) {
	candidates, err := r.candidatesByAllMethods(ctx, methodNames, index.KindType)
	if err != nil {
		return nil, err
	}

	out := make(map[store.MethodEntry]*types.Named)
	for entry := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cname, _, _, ok := r.symbolByHash(ctx, entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		cpath, ok := r.pkgPathByHash[entry.PkgHash]
		if !ok {
			continue
		}
		cnamed, err := r.resolveNamed(ctx, cpath, cname)
		if err != nil {
			continue
		}
		if !types.Implements(types.NewPointer(cnamed), iface) {
			continue
		}
		out[entry] = cnamed
	}
	return out, nil
}

// concreteMethodLocation returns the declaration location, in pkgHash's own
// facts, of methodName in named's pointer method set -- the same superset
// registerMethodSet indexed named's type entry under.
func (r *Resolver) concreteMethodLocation(ctx context.Context, pkgHash uint64, named *types.Named, methodName string) (Location, bool) {
	ms := types.NewMethodSet(types.NewPointer(named))
	for i := 0; i < ms.Len(); i++ {
		fn, ok := ms.At(i).Obj().(*types.Func)
		if !ok || fn.Name() != methodName {
			continue
		}
		return r.methodFuncLocation(ctx, pkgHash, fn)
	}
	return Location{}, false
}

// interfacesImplementedBy finds every interface in the workspace that named
// implements.
//
// A zero-method named type is symmetrically excluded: it technically
// implements every zero-method interface (chiefly interface{}/any), but
// implementationsOfInterface deliberately declines to report those
// implementers, so staying consistent here means returning no results too
// rather than the empty-method-name index having nothing to match against.
func (r *Resolver) interfacesImplementedBy(ctx context.Context, named *types.Named) ([]Location, error) {
	ms := types.NewMethodSet(types.NewPointer(named))
	if ms.Len() == 0 {
		return nil, nil
	}
	names := make([]string, ms.Len())
	for i := 0; i < ms.Len(); i++ {
		names[i] = ms.At(i).Obj().Name()
	}

	ifaces, err := r.implementedInterfaces(ctx, named, names)
	if err != nil {
		return nil, err
	}
	var out []Location
	for entry := range ifaces {
		_, _, loc, ok := r.symbolByHash(ctx, entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	return out, nil
}

// methodInterfaces is interfacesImplementedBy's method-granular
// counterpart, for an implementation query on named's methodName method
// itself rather than on named's name: it returns each satisfied
// interface's matching method location instead of the interface's own
// declaration location.
func (r *Resolver) methodInterfaces(ctx context.Context, named *types.Named, methodName string) ([]Location, error) {
	ms := types.NewMethodSet(types.NewPointer(named))
	names := make([]string, ms.Len())
	for i := 0; i < ms.Len(); i++ {
		names[i] = ms.At(i).Obj().Name()
	}

	ifaces, err := r.implementedInterfaces(ctx, named, names)
	if err != nil {
		return nil, err
	}
	var out []Location
	for entry, iface := range ifaces {
		loc, ok := r.interfaceMethodLocation(ctx, entry.PkgHash, iface, methodName)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	return out, nil
}

// implementedInterfaces unions [store.DB.LookupMethod] candidates across
// every name in methodNames (an interface named satisfies must have at
// least one method named also has), then confirms each survivor with
// types.Implements -- which also rejects the interfaces the first pass
// over-approximated -- returning the ones that pass alongside their
// resolved *types.Interface. ctx is checked once per candidate (see
// implementingTypes's doc).
func (r *Resolver) implementedInterfaces(ctx context.Context, named *types.Named, methodNames []string) (map[store.MethodEntry]*types.Interface, error) {
	candidates, err := r.candidatesByAnyMethod(ctx, methodNames, index.KindInterface)
	if err != nil {
		return nil, err
	}

	out := make(map[store.MethodEntry]*types.Interface)
	for entry := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		iname, _, _, ok := r.symbolByHash(ctx, entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		ipath, ok := r.pkgPathByHash[entry.PkgHash]
		if !ok {
			continue
		}
		inamed, err := r.resolveNamed(ctx, ipath, iname)
		if err != nil {
			continue
		}
		iface, ok := inamed.Underlying().(*types.Interface)
		if !ok {
			continue
		}
		if !types.Implements(types.NewPointer(named), iface) {
			continue
		}
		out[entry] = iface
	}
	return out, nil
}

// interfaceMethodLocation returns the declaration location, in pkgHash's
// own facts, of methodName among iface's directly declared methods -- the
// same set registerInterfaceMethodSet indexed iface's type entry under.
func (r *Resolver) interfaceMethodLocation(ctx context.Context, pkgHash uint64, iface *types.Interface, methodName string) (Location, bool) {
	for i := 0; i < iface.NumMethods(); i++ {
		fn := iface.Method(i)
		if fn.Name() != methodName {
			continue
		}
		return r.methodFuncLocation(ctx, pkgHash, fn)
	}
	return Location{}, false
}

// methodFuncLocation recomputes fn's own SymbolID the same way facts
// extraction did for its defining identifier (see index's symbolID) and
// looks it up in pkgHash's facts, giving fn's exact declaration position.
// This is necessary because fn -- decoded from export data via
// resolveNamed/resolveMethodFunc -- carries a token.Pos whose column
// information export data does not preserve; facts, built directly from
// the AST at index time, do.
func (r *Resolver) methodFuncLocation(ctx context.Context, pkgHash uint64, fn *types.Func) (Location, bool) {
	pkgPath, ok := r.pkgPathByHash[pkgHash]
	if !ok {
		return Location{}, false
	}
	enc := new(objectpath.Encoder)
	objPath, err := enc.For(fn)
	if err != nil {
		return Location{}, false
	}
	idHash := store.Hash(store.BuildSymbolID(pkgPath, string(objPath)))
	_, _, loc, ok := r.symbolByHash(ctx, pkgHash, idHash)
	return loc, ok
}

// candidatesByAllMethods returns every MethodEntry of kind wantKind recorded
// under every name in methodNames (intersection): a sound, name-based
// shortlist for "does this candidate implement an interface with these
// methods".
func (r *Resolver) candidatesByAllMethods(ctx context.Context, methodNames []string, wantKind uint8) (map[store.MethodEntry]bool, error) {
	var result map[store.MethodEntry]bool
	for i, name := range methodNames {
		set, err := r.methodEntriesOfKind(ctx, name, wantKind)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			result = set
			continue
		}
		for k := range result {
			if !set[k] {
				delete(result, k)
			}
		}
		if len(result) == 0 {
			return nil, nil
		}
	}
	return result, nil
}

// candidatesByAnyMethod returns every MethodEntry of kind wantKind recorded
// under any name in methodNames (union): a sound, name-based shortlist for
// "does this candidate's interface consist only of methods this type has".
func (r *Resolver) candidatesByAnyMethod(ctx context.Context, methodNames []string, wantKind uint8) (map[store.MethodEntry]bool, error) {
	result := make(map[store.MethodEntry]bool)
	for _, name := range methodNames {
		set, err := r.methodEntriesOfKind(ctx, name, wantKind)
		if err != nil {
			return nil, err
		}
		for k := range set {
			result[k] = true
		}
	}
	return result, nil
}

func (r *Resolver) methodEntriesOfKind(ctx context.Context, methodName string, wantKind uint8) (map[store.MethodEntry]bool, error) {
	entries, err := r.db.LookupMethod(ctx, methodName)
	if err != nil {
		return nil, err
	}
	set := make(map[store.MethodEntry]bool, len(entries))
	for _, e := range entries {
		_, kind, _, ok := r.symbolByHash(ctx, e.PkgHash, e.TypeSymbolIDHash)
		if ok && kind == wantKind {
			set[e] = true
		}
	}
	return set, nil
}
