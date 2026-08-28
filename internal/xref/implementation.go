package xref

import (
	"fmt"
	"go/types"

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
)

// Implementation returns, for an interface at (file, line, col), every
// concrete type in the workspace that implements it; for a concrete named
// type, every interface in the workspace it implements. Both directions use
// a sound name-based first pass over the method index followed by a
// types.Implements confirmation against export data (see package doc).
func (r *Resolver) Implementation(file string, line, col int) ([]Location, error) {
	target, err := r.resolveAt(file, uint32(line), uint32(col))
	if err != nil {
		return nil, err
	}
	pkgPath, ok := r.pkgPathByHash[target.PkgHash]
	if !ok {
		return nil, fmt.Errorf("xref: unknown defining package for hash %d", target.PkgHash)
	}
	named, err := r.resolveNamed(pkgPath, target.Name)
	if err != nil {
		return nil, err
	}

	switch target.Kind {
	case index.KindInterface:
		return r.implementationsOfInterface(named)
	case index.KindType:
		return r.interfacesImplementedBy(named)
	default:
		return nil, fmt.Errorf("xref: implementation query not supported for symbol kind %d", target.Kind)
	}
}

// implementationsOfInterface finds every concrete type in the workspace
// that implements the interface named. The first pass intersects
// [store.DB.LookupMethod] candidates across every one of the interface's
// method names (a real implementer must have all of them); the second pass
// confirms each survivor with types.Implements.
func (r *Resolver) implementationsOfInterface(named *types.Named) ([]Location, error) {
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("xref: %s is not an interface", named.Obj().Name())
	}
	names := make([]string, iface.NumMethods())
	for i := range names {
		names[i] = iface.Method(i).Name()
	}

	candidates, err := r.candidatesByAllMethods(names, index.KindType)
	if err != nil {
		return nil, err
	}

	var out []Location
	for entry := range candidates {
		cname, _, _, ok := r.symbolByHash(entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		cpath, ok := r.pkgPathByHash[entry.PkgHash]
		if !ok {
			continue
		}
		cnamed, err := r.resolveNamed(cpath, cname)
		if err != nil {
			continue
		}
		if !types.Implements(types.NewPointer(cnamed), iface) {
			continue
		}
		_, _, loc, ok := r.symbolByHash(entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	return out, nil
}

// interfacesImplementedBy finds every interface in the workspace that named
// implements. The first pass unions [store.DB.LookupMethod] candidates
// across named's method names (an interface named satisfies must have at
// least one method named also has); the second pass confirms each survivor
// with types.Implements, which also rejects the interfaces the first pass
// over-approximated.
func (r *Resolver) interfacesImplementedBy(named *types.Named) ([]Location, error) {
	ms := types.NewMethodSet(types.NewPointer(named))
	names := make([]string, ms.Len())
	for i := 0; i < ms.Len(); i++ {
		names[i] = ms.At(i).Obj().Name()
	}

	candidates, err := r.candidatesByAnyMethod(names, index.KindInterface)
	if err != nil {
		return nil, err
	}

	var out []Location
	for entry := range candidates {
		iname, _, _, ok := r.symbolByHash(entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		ipath, ok := r.pkgPathByHash[entry.PkgHash]
		if !ok {
			continue
		}
		inamed, err := r.resolveNamed(ipath, iname)
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
		_, _, loc, ok := r.symbolByHash(entry.PkgHash, entry.TypeSymbolIDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	return out, nil
}

// candidatesByAllMethods returns every MethodEntry of kind wantKind recorded
// under every name in methodNames (intersection): a sound, name-based
// shortlist for "does this candidate implement an interface with these
// methods".
func (r *Resolver) candidatesByAllMethods(methodNames []string, wantKind uint8) (map[store.MethodEntry]bool, error) {
	var result map[store.MethodEntry]bool
	for i, name := range methodNames {
		set, err := r.methodEntriesOfKind(name, wantKind)
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
func (r *Resolver) candidatesByAnyMethod(methodNames []string, wantKind uint8) (map[store.MethodEntry]bool, error) {
	result := make(map[store.MethodEntry]bool)
	for _, name := range methodNames {
		set, err := r.methodEntriesOfKind(name, wantKind)
		if err != nil {
			return nil, err
		}
		for k := range set {
			result[k] = true
		}
	}
	return result, nil
}

func (r *Resolver) methodEntriesOfKind(methodName string, wantKind uint8) (map[store.MethodEntry]bool, error) {
	entries, err := r.db.LookupMethod(methodName)
	if err != nil {
		return nil, err
	}
	set := make(map[store.MethodEntry]bool, len(entries))
	for _, e := range entries {
		_, kind, _, ok := r.symbolByHash(e.PkgHash, e.TypeSymbolIDHash)
		if ok && kind == wantKind {
			set[e] = true
		}
	}
	return set, nil
}
