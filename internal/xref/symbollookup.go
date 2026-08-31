package xref

import "github.com/sivchari/golance/internal/store"

// TypeDeclaration returns the declaration location of the object identified
// by (pkgPath, objPath) — an objectpath.Path string computed the same way
// internal/index's facts extraction computes it (see store.BuildSymbolID)
// — for resolving textDocument/typeDefinition when the type lives in a
// different package than the query (see internal/langfeat.TypeDefinition).
// ok is false if pkgPath's facts are not indexed, or no symbol in them
// matches.
func (r *Resolver) TypeDeclaration(pkgPath, objPath string) (Location, bool) {
	idHash := store.Hash(store.BuildSymbolID(pkgPath, objPath))
	_, _, loc, ok := r.symbolByHash(store.Hash(pkgPath), idHash)
	return loc, ok
}

// SymbolDoc returns the doc comment recorded for the object identified by
// (pkgPath, objPath), for resolving completionItem/resolve when the
// candidate is declared in a different package than the query (see
// internal/langfeat.ResolveCompletionDoc). ok is false under the same
// conditions as TypeDeclaration.
func (r *Resolver) SymbolDoc(pkgPath, objPath string) (string, bool) {
	u, err := r.unitBlob(store.Hash(pkgPath))
	if err != nil {
		return "", false
	}
	v, err := store.NewView(u.Facts)
	if err != nil {
		return "", false
	}
	s, ok := v.LookupSymbol(store.Hash(store.BuildSymbolID(pkgPath, objPath)))
	if !ok {
		return "", false
	}
	return s.Doc(), true
}
