package xref

import (
	"context"
	"fmt"
	"go/types"
	"strings"

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
	named, err := r.methodReceiver(ctx, pkgPath, target)
	if err != nil {
		return nil, err
	}
	if types.IsInterface(named) {
		return r.methodImplementations(ctx, named, target.Name)
	}
	return r.methodInterfaces(ctx, named, target.Name)
}

// methodReceiver resolves target's own *types.Func (via resolveMethodFunc)
// and returns its receiver's named type, shared by implementationOfMethod's
// dispatch above and correspondingMethodSymbols' below -- both need to
// classify target's receiver as an interface or a concrete type before
// picking a direction to search in.
func (r *Resolver) methodReceiver(ctx context.Context, pkgPath string, target resolvedSymbol) (*types.Named, error) {
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
	return named, nil
}

// correspondingMethodSymbols is implementationOfMethod's resolvedSymbol
// counterpart, for References (see xref.go): given a method target, it
// returns the matching method of every type on the other side of the same
// interface/implementer relationship implementationOfMethod would list
// Locations for, as resolvedSymbols a caller can feed back into
// locationsFor. Both directions are wired in: an interface method's
// implementers (methodImplementationSymbols) and, symmetrically, a concrete
// method's satisfied interfaces (interfacesSatisfiedByMethod) -- see the
// latter's doc for why that direction no longer carries the cost that used
// to keep it out of References.
func (r *Resolver) correspondingMethodSymbols(ctx context.Context, target resolvedSymbol) ([]resolvedSymbol, error) {
	pkgPath, ok := r.pkgPathByHash[target.PkgHash]
	if !ok {
		return nil, fmt.Errorf("xref: unknown defining package for hash %d", target.PkgHash)
	}
	named, err := r.methodReceiver(ctx, pkgPath, target)
	if err != nil {
		return nil, err
	}
	if types.IsInterface(named) {
		return r.methodImplementationSymbols(ctx, named, target.Name)
	}
	return r.interfacesSatisfiedByMethod(ctx, named, target.Name)
}

// interfacesSatisfiedByMethod is correspondingMethodSymbols' concrete ->
// interfaces direction: given named's own methodName method, it returns the
// matching method of every workspace interface named satisfies that also
// declares a method by this name -- what a call through an interface-typed
// variable resolves to, and so what References on the concrete method must
// union in alongside target's own direct call sites.
//
// Candidate gathering is deliberately bounded to a SINGLE
// [store.DB.LookupMethod] posting list (methodName alone), unlike
// methodInterfaceSymbols/implementedInterfaces (Implementation's own
// concrete -> interfaces query, which unions candidates across EVERY name in
// named's whole method set): that whole-method-set union is exactly the
// unbounded cost References' package doc used to cite for deferring this
// direction entirely. A single name is bounded by whatever the workspace
// declares under it, the same bound implementingTypes' interface ->
// implementers direction already relies on, and is cheap enough for
// References' hot path.
//
// Confirmation still decodes each candidate's export data and calls
// types.Implements, exactly as implementedInterfaces: unlike a queried
// interface (already live and decoded by the time implementingTypes runs),
// a LookupMethod candidate here is only known to have SOME method named
// methodName -- its full method set (whether it needs anything else besides)
// is not recoverable from the index without decoding it, so there is no
// index-only fingerprint shortcut available on this side either, the same
// reasoning implementedInterfaces' own doc gives for staying decode-based.
// That decode is affordable now: r.unitBlob/r.cache cache every export data
// decode for this Resolver's whole lifetime (see unitCache's doc and
// package doc), so a candidate package already visited elsewhere in the
// same query -- or a shared dependency two candidates both import -- decodes
// only once. Generic candidates and generic receivers fall back to the same
// types.Implements call as the non-generic case, exactly like
// methodInterfaceSymbols/implementedInterfaces already do -- no separate
// generic handling needed here.
//
// Unlike Implementation's own queries, an empty result here is the
// overwhelmingly common case (most methods satisfy no interface at all),
// so -- unlike implementedInterfaces -- this deliberately skips implDiag/
// logImplDiag: logging every miss would be log noise on References' hot
// path, not the rare, actionable signal implDiag exists for. ctx is checked
// once per candidate, mirroring implementedInterfaces' own loop.
func (r *Resolver) interfacesSatisfiedByMethod(ctx context.Context, named *types.Named, methodName string) ([]resolvedSymbol, error) {
	diag := newImplDiag([]string{methodName})
	candidates, err := r.methodEntriesOfKind(ctx, methodName, index.KindInterface, diag)
	if err != nil {
		return nil, err
	}

	var out []resolvedSymbol
	for key := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		iname, _, _, ok := r.symbolByHash(ctx, key.PkgHash, key.TypeSymbolIDHash)
		if !ok {
			continue
		}
		ipath, ok := r.pkgPathByHash[key.PkgHash]
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
		sym, ok := r.interfaceMethodSymbol(iface, methodName)
		if !ok {
			continue
		}
		out = append(out, sym)
	}
	return out, nil
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
		r.logDecodeFailureOnce(pkgPath, err)
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

	diag := newImplDiag(names)
	impls, err := r.implementingTypes(ctx, named, iface, names, diag)
	if err != nil {
		return nil, err
	}
	var out []Location
	for key := range impls {
		_, _, loc, ok := r.symbolByHash(ctx, key.PkgHash, key.TypeSymbolIDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	if len(out) == 0 {
		r.logImplDiag("implementations of interface "+named.Obj().Name(), diag)
	}
	return out, nil
}

// methodImplementations is implementationsOfInterface's method-granular
// counterpart, for an implementation query on iface's methodName method
// itself rather than on iface's name: it returns each implementer's
// matching method location instead of the implementer type's own
// declaration location.
func (r *Resolver) methodImplementations(ctx context.Context, iface *types.Named, methodName string) ([]Location, error) {
	syms, err := r.methodImplementationSymbols(ctx, iface, methodName)
	if err != nil {
		return nil, err
	}
	return r.locationsOfSymbols(ctx, syms), nil
}

// methodImplementationSymbols is methodImplementations' resolvedSymbol
// counterpart: every implementer's matching method as a resolvedSymbol
// rather than its declaration Location, for feeding into
// correspondingMethodSymbols/locationsFor (see xref.go's References).
func (r *Resolver) methodImplementationSymbols(ctx context.Context, iface *types.Named, methodName string) ([]resolvedSymbol, error) {
	ifaceType, ok := iface.Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("xref: %s is not an interface", iface.Obj().Name())
	}
	names := make([]string, ifaceType.NumMethods())
	for i := range names {
		names[i] = ifaceType.Method(i).Name()
	}

	diag := newImplDiag(names)
	impls, err := r.implementingTypes(ctx, iface, ifaceType, names, diag)
	if err != nil {
		return nil, err
	}
	var out []resolvedSymbol
	for _, byName := range impls {
		sym, ok := candidateMethodSymbol(byName, methodName)
		if !ok {
			continue
		}
		out = append(out, sym)
	}
	if len(out) == 0 {
		r.logImplDiag(fmt.Sprintf("implementations of %s.%s", iface.Obj().Name(), methodName), diag)
	}
	return out, nil
}

// candidateKey identifies one indexed type (interface or concrete)
// independent of which method name a query happened to find it under: the
// same (PkgHash, TypeSymbolIDHash) pair recurs across every method-name
// bucket a multi-method type is indexed under, but a store.MethodEntry's
// OTHER fields (MethodIDHash, Fingerprint, ...) are specific to one single
// method -- grouping by candidateKey alone, instead of by whole
// store.MethodEntry values, is what makes intersecting/unioning
// [store.DB.LookupMethod] results across several method names correct (see
// candidatesByAllMethods/candidatesByAnyMethod).
type candidateKey struct {
	PkgHash          uint64
	TypeSymbolIDHash uint64
}

// candidateOccurrences maps a candidate type to the store.MethodEntry
// records recorded for it, per required method name (one query's worth).
// There is usually exactly one occurrence per name; more than one means a
// type's method signature changed since an earlier index write and the old
// entry was never removed (see applyIndexEntries's doc) -- confirmation
// treats any matching occurrence as sufficient, never requiring all of them
// to agree.
type candidateOccurrences map[candidateKey]map[string][]store.MethodEntry

// implementingTypes intersects [store.DB.LookupMethod] candidates across
// every one of methodNames (a real implementer must have all of them, so a
// candidate missing even one is never confirmed), then confirms each
// survivor by comparing each required method's canonical signature
// fingerprint against ifaceNamed's own (see internal/index's
// MethodFingerprint/registerMethodSet and [store.MethodEntry]'s doc) --
// which needs no export-data decode of the candidate at all. That is what
// makes this direction work for an unexported implementer (see the package
// doc's "unexported implementer" note): export data only ever carries
// exported package-scope objects, so a candidate whose type is itself
// unexported used to be dropped here no matter how genuinely it implemented
// the interface, since resolveNamed's lookup against decoded export data
// simply never finds it.
//
// Soundness: types.TypeString of a method's *types.Signature never prints
// its receiver, and MethodFingerprint renders every named type in that
// signature -- including the method's own home package -- by its full
// import path rather than blanking whichever package happens to be
// "current" (see index.fingerprintQualifier's doc). Two methods (an
// interface's declared method, and a candidate's) whose names and canonical
// renderings match are therefore the same method signature by
// construction: an incorrect MATCH would require a genuine collision in
// store.Hash's 64-bit space, the same accepted risk its own doc already
// carries for every other hash this codebase persists (PkgHash, IDHash,
// ContentHash, ...). A concrete type satisfies an interface once it has ALL
// of the interface's methods with matching signatures; it may freely have
// MORE methods besides, so -- unlike implementedInterfaces, the reverse
// direction (see its own doc for why that one keeps the decode-based
// confirmation instead) -- there is no "does the candidate also require
// extra methods" concern to account for here: comparing exactly iface's own
// methodNames is already a complete check, so fingerprint confirmation is
// authoritative on its own rather than a mere fast path.
//
// A candidate only falls back to the pre-fix decode-and-types.Implements
// confirmation when fingerprint confirmation cannot be trusted for it: the
// interface itself is generic (ifaceGeneric, mirroring registerMethodSet's
// same exclusion for a generic receiver), or one of the candidate's own
// recorded fingerprints is 0 (its receiver is generic) or simply does not
// match iface's -- which can mean either "genuinely does not implement" or
// "the index has a stale entry from a since-changed signature" (see
// candidateOccurrences' doc), so it is never treated as a confirmed
// rejection on its own. ctx is checked once per candidate that reaches the
// decode fallback: a canceled query stops before decoding the next
// candidate's export data (the expensive part of this loop) instead of
// running to completion regardless.
func (r *Resolver) implementingTypes(ctx context.Context, ifaceNamed *types.Named, iface *types.Interface, methodNames []string, diag *implDiag) (candidateOccurrences, error) {
	ifaceGeneric := ifaceNamed.TypeParams().Len() > 0
	ifaceFPs := make(map[string]uint64, len(methodNames))
	if !ifaceGeneric {
		for i := 0; i < iface.NumMethods(); i++ {
			fn := iface.Method(i)
			if sig, ok := fn.Type().(*types.Signature); ok {
				ifaceFPs[fn.Name()] = index.MethodFingerprint(sig)
			}
		}
	}

	candidates, err := r.candidatesByAllMethods(ctx, methodNames, index.KindType, diag)
	if err != nil {
		return nil, err
	}

	out := make(candidateOccurrences, len(candidates))
	for key, byName := range candidates {
		if !ifaceGeneric && fingerprintsConfirm(byName, methodNames, ifaceFPs) {
			out[key] = byName
			diag.survivors++
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cname, _, _, ok := r.symbolByHash(ctx, key.PkgHash, key.TypeSymbolIDHash)
		if !ok {
			continue
		}
		cpath, ok := r.pkgPathByHash[key.PkgHash]
		if !ok {
			continue
		}
		cnamed, err := r.resolveNamed(ctx, cpath, cname)
		if err != nil {
			diag.skip(cpath, cname, err)
			continue
		}
		if !types.Implements(types.NewPointer(cnamed), iface) {
			diag.fingerprintMismatch++
			continue
		}
		out[key] = byName
		diag.survivors++
	}
	return out, nil
}

// fingerprintsConfirm reports whether byName -- one candidate's recorded
// store.MethodEntry occurrences, per method name (see candidateOccurrences'
// doc) -- has, for every name in methodNames, at least one occurrence whose
// Fingerprint matches ifaceFPs' entry for that same name and is non-zero
// (see store.MethodEntry's doc for why 0, the generic-receiver sentinel,
// never counts as a match). candidatesByAllMethods already guarantees
// byName has SOME occurrence for every name in methodNames; this only asks
// whether any of them actually matches.
func fingerprintsConfirm(byName map[string][]store.MethodEntry, methodNames []string, ifaceFPs map[string]uint64) bool {
	for _, name := range methodNames {
		want, ok := ifaceFPs[name]
		if !ok || want == 0 || !anyFingerprintMatches(byName[name], want) {
			return false
		}
	}
	return true
}

func anyFingerprintMatches(entries []store.MethodEntry, want uint64) bool {
	for _, e := range entries {
		if e.Fingerprint == want {
			return true
		}
	}
	return false
}

// candidateMethodSymbol picks byName's own recorded occurrence for
// methodName -- one confirmed implementer's store.MethodEntry data (see
// candidateOccurrences' doc) -- and resolves it directly to a
// resolvedSymbol: MethodPkgHash/MethodIDHash already identify the method's
// own declaration (see store.MethodEntry's doc), computed once at index
// time by internal/index's methodEntrySelf, so no export data or live
// *types.Named is needed here at all -- the decode-based
// concreteMethodSymbol/methodFuncSymbol pairing this replaces used to be
// the only way to resolve a candidate's specific method location, which is
// exactly what made it fail for an unexported implementer.
func candidateMethodSymbol(byName map[string][]store.MethodEntry, methodName string) (resolvedSymbol, bool) {
	entries := byName[methodName]
	if len(entries) == 0 {
		return resolvedSymbol{}, false
	}
	e := entries[0]
	return resolvedSymbol{PkgHash: e.MethodPkgHash, IDHash: e.MethodIDHash, Kind: index.KindMethod, Name: methodName}, true
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

	diag := newImplDiag(names)
	ifaces, err := r.implementedInterfaces(ctx, named, names, diag)
	if err != nil {
		return nil, err
	}
	var out []Location
	for key := range ifaces {
		_, _, loc, ok := r.symbolByHash(ctx, key.PkgHash, key.TypeSymbolIDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	sortLocations(out)
	if len(out) == 0 {
		r.logImplDiag("interfaces implemented by "+named.Obj().Name(), diag)
	}
	return out, nil
}

// methodInterfaces is interfacesImplementedBy's method-granular
// counterpart, for an implementation query on named's methodName method
// itself rather than on named's name: it returns each satisfied
// interface's matching method location instead of the interface's own
// declaration location.
func (r *Resolver) methodInterfaces(ctx context.Context, named *types.Named, methodName string) ([]Location, error) {
	syms, err := r.methodInterfaceSymbols(ctx, named, methodName)
	if err != nil {
		return nil, err
	}
	return r.locationsOfSymbols(ctx, syms), nil
}

// methodInterfaceSymbols is methodInterfaces' resolvedSymbol counterpart:
// every satisfied interface's matching method as a resolvedSymbol rather
// than its declaration Location.
//
// When methodName is a method a satisfied interface only has via embedding
// the builtin error (e.g. querying "Implementation" on a concrete type's
// own Error method, where the satisfied interface is `interface { error;
// ... }`), interfaceMethodSymbol's underlying methodFuncSymbol call
// deliberately returns no match for that interface: the universe's own
// error.Error has no declaring package and so no location in the workspace
// to point at (see methodFuncSymbol's doc). That interface is silently
// skipped here rather than included with a made-up or missing Location —
// this only affects the method-granular query; querying "Implementation" on
// the concrete type's own NAME still finds the interface via
// interfacesImplementedBy, which resolves the interface's own declaration
// Location and never needs to resolve the individual method's.
func (r *Resolver) methodInterfaceSymbols(ctx context.Context, named *types.Named, methodName string) ([]resolvedSymbol, error) {
	ms := types.NewMethodSet(types.NewPointer(named))
	names := make([]string, ms.Len())
	for i := 0; i < ms.Len(); i++ {
		names[i] = ms.At(i).Obj().Name()
	}

	diag := newImplDiag(names)
	ifaces, err := r.implementedInterfaces(ctx, named, names, diag)
	if err != nil {
		return nil, err
	}
	var out []resolvedSymbol
	for _, iface := range ifaces {
		sym, ok := r.interfaceMethodSymbol(iface, methodName)
		if !ok {
			continue
		}
		out = append(out, sym)
	}
	if len(out) == 0 {
		r.logImplDiag(fmt.Sprintf("interfaces satisfied by %s.%s", named.Obj().Name(), methodName), diag)
	}
	return out, nil
}

// implementedInterfaces unions [store.DB.LookupMethod] candidates across
// every name in methodNames (an interface named satisfies must have at
// least one method named also has), then confirms each survivor with
// types.Implements -- which also rejects the interfaces the first pass
// over-approximated -- returning the ones that pass alongside their
// resolved *types.Interface.
//
// Unlike implementingTypes' interface -> implementers direction, this stays
// on the decode-based confirmation deliberately: unioning only named's own
// method names finds every candidate interface that shares AT LEAST ONE
// name with named, but never rules out a candidate that additionally
// requires some OTHER method named does not have -- only fully decoding the
// candidate's own method set (via resolveNamed, then types.Implements)
// answers that. Fingerprint-confirming this direction soundly would need an
// index of each interface's TOTAL method count (or full name set) to bound
// it, which is disproportionate scope for a direction the reported bug
// never actually implicated: the "unexported struct implements an exported
// interface" pattern this fix targets means the candidates on THIS side are
// interfaces, conventionally exported and so already decodable in practice.
// ctx is checked once per candidate, mirroring implementingTypes' decode
// fallback.
func (r *Resolver) implementedInterfaces(ctx context.Context, named *types.Named, methodNames []string, diag *implDiag) (map[candidateKey]*types.Interface, error) {
	candidates, err := r.candidatesByAnyMethod(ctx, methodNames, index.KindInterface, diag)
	if err != nil {
		return nil, err
	}

	out := make(map[candidateKey]*types.Interface)
	for key := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		iname, _, _, ok := r.symbolByHash(ctx, key.PkgHash, key.TypeSymbolIDHash)
		if !ok {
			continue
		}
		ipath, ok := r.pkgPathByHash[key.PkgHash]
		if !ok {
			continue
		}
		inamed, err := r.resolveNamed(ctx, ipath, iname)
		if err != nil {
			diag.skip(ipath, iname, err)
			continue
		}
		iface, ok := inamed.Underlying().(*types.Interface)
		if !ok {
			continue
		}
		if !types.Implements(types.NewPointer(named), iface) {
			continue
		}
		out[key] = iface
	}
	diag.survivors += len(out)
	return out, nil
}

// interfaceMethodSymbol resolves methodName among iface's directly declared
// methods -- the same set registerInterfaceMethodSet indexed iface's type
// entry under -- to its own resolvedSymbol. Like concreteMethodSymbol, this
// is deliberately NOT keyed by iface's own package: [types.Interface.
// Method] flattens methods promoted from an embedded interface without
// changing their Pkg(), which stays the embedded interface's own defining
// package (see methodFuncSymbol).
func (r *Resolver) interfaceMethodSymbol(iface *types.Interface, methodName string) (resolvedSymbol, bool) {
	for i := 0; i < iface.NumMethods(); i++ {
		fn := iface.Method(i)
		if fn.Name() != methodName {
			continue
		}
		return r.methodFuncSymbol(fn)
	}
	return resolvedSymbol{}, false
}

// methodFuncSymbol resolves fn to the resolvedSymbol facts extraction
// recorded for it, by recomputing its SymbolID the same way index's
// symbolID did for its defining identifier: fn.Pkg().Path() plus its
// objectpath, exactly as symbolID built it from obj.Pkg().Path() (see
// index/facts.go). Deriving the package from fn itself -- rather than from
// whatever candidate/interface type's method set fn was reached through --
// is what makes this correct for a promoted method: [types.MethodSet] and
// [types.Interface.Method] both return promoted methods as the same *Func
// object their embedding type/interface originally declared, so fn.Pkg()
// is that ORIGINAL declaring package, not the embedder's. Using the
// embedder's package instead (as this used to) builds a SymbolID nothing
// was ever indexed under, silently dropping every implementation/reference
// that only exists via struct or interface embedding.
//
// fn.Pkg() is nil for a method belonging to the predeclared universe scope
// (e.g. error's Error method, reached whenever fn was promoted from an
// embedded builtin error). Such a method has no declaring package and so no
// indexed declaration to resolve to; report no match instead of panicking
// on fn.Pkg().Path(), mirroring internal/index's methodEntrySelf, which
// leaves the very same case unresolved at index time.
func (r *Resolver) methodFuncSymbol(fn *types.Func) (resolvedSymbol, bool) {
	pkg := fn.Pkg()
	if pkg == nil {
		return resolvedSymbol{}, false
	}
	pkgPath := pkg.Path()
	enc := new(objectpath.Encoder)
	objPath, err := enc.For(fn)
	if err != nil {
		return resolvedSymbol{}, false
	}
	return resolvedSymbol{
		PkgHash: store.Hash(pkgPath),
		IDHash:  store.Hash(store.BuildSymbolID(pkgPath, string(objPath))),
		Kind:    index.KindMethod,
		Name:    fn.Name(),
	}, true
}

// locationsOfSymbols resolves each of syms to its declaration Location via
// symbolByHash, silently dropping any that no longer resolve (e.g. a
// candidate whose package fell out of the indexed workspace between the
// symbol lookup and this call), and returns them deduplicated and sorted.
// Deduplication matters here specifically: two distinct implementers can
// resolve the SAME method name to the identical underlying declaration --
// e.g. a helper type that both (a) gets embedded into some implementer,
// contributing this method to it by promotion, and (b) happens to satisfy
// the interface entirely on its own too -- so the candidate list this
// builds from can legitimately contain two different concrete types whose
// resolvedSymbol for methodName is nonetheless the same Func.
func (r *Resolver) locationsOfSymbols(ctx context.Context, syms []resolvedSymbol) []Location {
	var out []Location
	for _, s := range syms {
		_, _, loc, ok := r.symbolByHash(ctx, s.PkgHash, s.IDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	out = dedupeLocations(out)
	sortLocations(out)
	return out
}

// candidatesByAllMethods returns, for every candidate of kind wantKind that
// has ALL of methodNames recorded (intersection), its store.MethodEntry
// occurrences per name (see candidateOccurrences' doc): a sound, name-based
// shortlist for "does this candidate implement an interface with these
// methods".
func (r *Resolver) candidatesByAllMethods(ctx context.Context, methodNames []string, wantKind uint8, diag *implDiag) (candidateOccurrences, error) {
	result := make(candidateOccurrences)
	for i, name := range methodNames {
		set, err := r.methodEntriesOfKind(ctx, name, wantKind, diag)
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

// candidatesByAnyMethod returns, for every candidate of kind wantKind that
// has AT LEAST ONE of methodNames recorded (union), its store.MethodEntry
// occurrences per name it does have: a sound, name-based shortlist for
// "does this candidate's interface consist only of methods this type has".
func (r *Resolver) candidatesByAnyMethod(ctx context.Context, methodNames []string, wantKind uint8, diag *implDiag) (candidateOccurrences, error) {
	result := make(candidateOccurrences)
	for _, name := range methodNames {
		set, err := r.methodEntriesOfKind(ctx, name, wantKind, diag)
		if err != nil {
			return nil, err
		}
		for k, entries := range set {
			if result[k] == nil {
				result[k] = make(map[string][]store.MethodEntry)
			}
			result[k][name] = entries
		}
	}
	return result, nil
}

// methodEntriesOfKind returns every store.MethodEntry [store.DB.LookupMethod]
// records for methodName whose receiver/interface type has kind wantKind,
// grouped by candidateKey (see its doc for why a bare MethodEntry cannot
// serve as that grouping key once it carries per-method fields).
//
// Both the candidate type (e.PkgHash/e.TypeSymbolIDHash) and the method
// itself (e.MethodPkgHash/e.MethodIDHash) are re-checked against the
// CURRENT facts index before an entry is trusted: applyIndexEntries's
// posting lists are append-only (a stale entry from a since-changed
// signature is "never removed", see its doc), which used to be harmless
// because decode-time types.Implements always re-confirmed against live
// data regardless of what the name-based first pass over-approximated. Now
// that implementingTypes can confirm a candidate from Fingerprint alone
// (see its doc), a stale entry left behind by, say, removing a method
// entirely (the method's own definition — and so its MethodIDHash — no
// longer resolves in its package's current facts, even though the
// candidate TYPE itself still exists) must be filtered out HERE, at the
// same sound first pass, rather than surviving into a confirmation step
// that no longer necessarily decodes anything to catch it.
func (r *Resolver) methodEntriesOfKind(ctx context.Context, methodName string, wantKind uint8, diag *implDiag) (map[candidateKey][]store.MethodEntry, error) {
	entries, err := r.db.LookupMethod(ctx, methodName)
	if err != nil {
		return nil, err
	}
	diag.recordLookup(methodName, len(entries))
	set := make(map[candidateKey][]store.MethodEntry, len(entries))
	for _, e := range entries {
		_, kind, _, ok := r.symbolByHash(ctx, e.PkgHash, e.TypeSymbolIDHash)
		if !ok || kind != wantKind {
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

// implDiag accumulates per-query diagnostics for an implementation query,
// so that when its final result comes back empty, logImplDiag can explain
// why -- the client's own view of an empty result is otherwise identical
// whether that means "genuinely no implementers" or a resolution step
// silently failing along the way. This is what closed the exact gap a real
// monorepo report hit: an interface whose method signatures reference a
// module dependency's type (gorm.io/gorm's *gorm.DB) kept answering "no
// implementation found" with nothing in the server log explaining why --
// every candidate's resolveNamed call was failing to decode that
// dependency's export data, silently dropped by a bare `continue`. Nothing
// here is client-visible; see Resolver.SetLogger.
type implDiag struct {
	methodNames         []string       // the interface/type's own method set, in query order
	perName             map[string]int // LookupMethod's raw candidate count per method name, before kind filtering
	skipped             []string       // "pkgPath.name: err" for each candidate whose export data failed to decode (the decode-fallback path only -- see implementingTypes/implementedInterfaces)
	survivors           int            // candidates confirmed (by fingerprint or by types.Implements), across every implementingTypes/implementedInterfaces call this diag was threaded through
	fingerprintMismatch int            // implementingTypes candidates whose fingerprints did not confirm a match and, after falling back to decoding them, types.Implements also said no -- distinct from skipped: this candidate WAS resolvable, it just genuinely does not implement
}

// newImplDiag starts an implDiag for a query over methodNames.
func newImplDiag(methodNames []string) *implDiag {
	return &implDiag{methodNames: methodNames, perName: make(map[string]int, len(methodNames))}
}

// recordLookup records name's raw LookupMethod candidate count.
func (d *implDiag) recordLookup(name string, count int) {
	d.perName[name] = count
}

// skip records that pkgPath's name candidate was dropped because its
// export data could not be decoded -- implementingTypes/implementedInterfaces
// call this in place of the bare `continue` they used to silently take.
func (d *implDiag) skip(pkgPath, name string, err error) {
	d.skipped = append(d.skipped, fmt.Sprintf("%s.%s: %v", pkgPath, name, err))
}

// logImplDiag emits diag's summary via r.logger (a no-op if unset; see
// SetLogger) for queryDesc's empty result: one line of counts always, plus
// a second line listing every export-data decode failure only when there
// was at least one -- the specific, actionable signal for the monorepo
// symptom implDiag's own doc describes. Two lines at most, and only when a
// query already came back empty, keeps this from ever becoming log noise
// on the overwhelmingly common non-empty path.
//
// fingerprint-mismatches is reported distinctly from the second line's
// undecodable-skip count (see implDiag's doc): the former means a candidate
// was resolved and genuinely rejected (fingerprint or, after falling back,
// types.Implements said no), the latter means resolution itself failed
// before any confirmation could run at all -- collapsing the two would
// re-hide exactly the distinction the original monorepo report needed.
func (r *Resolver) logImplDiag(queryDesc string, diag *implDiag) {
	if r.logger == nil {
		return
	}
	r.logger.Printf("xref: %s found no results: methods=%v lookup-candidates-by-name=%v post-confirmation-survivors=%d fingerprint-mismatches=%d",
		queryDesc, diag.methodNames, diag.perName, diag.survivors, diag.fingerprintMismatch)
	if len(diag.skipped) > 0 {
		r.logger.Printf("xref: %s: %d candidate(s) skipped, export data undecodable: %s",
			queryDesc, len(diag.skipped), strings.Join(diag.skipped, "; "))
	}
}
