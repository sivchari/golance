package index

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"math"
	"sort"

	"golang.org/x/tools/go/types/objectpath"

	"github.com/sivchari/golance/internal/store"
)

// Symbol kinds recorded in [store.SymbolInput.Kind].
const (
	KindFunc uint8 = iota
	KindMethod
	KindType
	KindInterface
	KindVar
	KindConst
	KindField
)

// FlagExported marks a symbol whose name is exported, in [store.SymbolInput.Flags].
const FlagExported uint8 = 1 << 0

// extractFacts walks tpkg's type-check result, populating b with its symbol
// definitions and outgoing references, and returning the name index, method
// index, and SymbolID string entries produced along the way. files must be
// the parsed *ast.File values for pkgPath, in the same order as fileList
// (used to build the FileIdx each symbol/ref record points into). When
// relative is set, the file table b.SetFiles writes stores each path
// relative to root instead of fileList's own absolute form (see
// Options.RelativePaths); the FileIdx lookup below still keys by fileList's
// original absolute paths, since that is what fset positions carry.
//
// The returned entries belong in the same PutPackage/PutPackagesBatch
// transaction as the rest of the package's data (see [store.UnitEntry].
// Index): a package can produce thousands of them (one per definition and
// reference), and writing each in its own commit would serialize on
// bbolt's single writer across every concurrent Build worker.
func extractFacts(fset *token.FileSet, pkgHash uint64, tpkg *types.Package, info *types.Info, files []*ast.File, fileList []string, b *store.Builder, root string, relative bool) store.PackageIndexEntries {
	storedFiles := fileList
	if relative {
		storedFiles = make([]string, len(fileList))
		for i, f := range fileList {
			storedFiles[i] = relPath(root, f)
		}
	}
	b.SetFiles(storedFiles)

	fileIdx := make(map[string]uint32, len(fileList))
	for i, f := range fileList {
		fileIdx[f] = uint32(i)
	}

	docs := make(map[*ast.Ident]string)
	for _, f := range files {
		collectDocsInto(f, docs)
	}

	enc := new(objectpath.Encoder)
	var idx store.PackageIndexEntries

	addDefs(fset, pkgHash, tpkg, info, fileIdx, docs, enc, b, &idx)
	addRefs(fset, info, fileIdx, enc, b, &idx)
	return idx
}

// defEntry pairs a defining identifier with the object it declares, sorted
// by position for a deterministic symbol table.
type defEntry struct {
	ident *ast.Ident
	obj   types.Object
}

// addDefs adds one store symbol per definition in info.Defs, in source
// position order.
func addDefs(fset *token.FileSet, pkgHash uint64, tpkg *types.Package, info *types.Info, fileIdx map[string]uint32, docs map[*ast.Ident]string, enc *objectpath.Encoder, b *store.Builder, idx *store.PackageIndexEntries) {
	defs := make([]defEntry, 0, len(info.Defs))
	for id, obj := range info.Defs {
		if obj == nil || isSkippedObject(obj) {
			continue
		}
		defs = append(defs, defEntry{id, obj})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].ident.Pos() < defs[j].ident.Pos() })

	for _, d := range defs {
		addDef(fset, pkgHash, tpkg, fileIdx, docs, enc, b, idx, d.ident, d.obj)
	}
}

// addDef records one symbol definition, its name/method indexing, and (for
// a non-interface named type) its method set.
func addDef(fset *token.FileSet, pkgHash uint64, tpkg *types.Package, fileIdx map[string]uint32, docs map[*ast.Ident]string, enc *objectpath.Encoder, b *store.Builder, idx *store.PackageIndexEntries, ident *ast.Ident, obj types.Object) {
	pos := fset.Position(ident.Pos())
	fi, ok := fileIdx[pos.Filename]
	if !ok {
		return
	}

	sid := symbolID(obj, enc, fset)
	idHash := store.Hash(sid)
	kind, flags := classify(obj)

	b.AddSymbol(&store.SymbolInput{
		IDHash:  idHash,
		Kind:    kind,
		Flags:   flags,
		Name:    obj.Name(),
		Doc:     docs[ident],
		Sig:     types.ObjectString(obj, qualifier(tpkg)),
		FileIdx: fi,
		Line:    u32pos(pos.Line),
		Col:     u32pos(pos.Column),
	})

	idx.SymStrs = append(idx.SymStrs, store.SymStrEntry{IDHash: idHash, SymbolID: sid})
	if obj.Exported() {
		idx.Names = append(idx.Names, store.NameEntry{Name: obj.Name(), IDHash: idHash})
	}

	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}
	if types.IsInterface(named) {
		registerInterfaceMethodSet(idx, pkgHash, idHash, named)
		return
	}
	registerMethodSet(idx, pkgHash, idHash, named)
}

// u32pos converts a go/token.Position field (Line, Column) — always
// non-negative and never remotely close to 4 GiB for a valid position —
// to uint32, clamping to 0 instead of wrapping around if it is somehow
// out of range.
func u32pos(n int) uint32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxUint32 {
		return 0
	}
	return uint32(n)
}

// addRefs adds one store ref per outgoing identifier and selector-field/method
// use recorded in info.
func addRefs(fset *token.FileSet, info *types.Info, fileIdx map[string]uint32, enc *objectpath.Encoder, b *store.Builder, idx *store.PackageIndexEntries) {
	for id, obj := range info.Uses {
		addRef(fset, fileIdx, enc, b, idx, fset.Position(id.Pos()), fset.Position(id.End()), obj)
	}
	for sel, selection := range info.Selections {
		addRef(fset, fileIdx, enc, b, idx, fset.Position(sel.Sel.Pos()), fset.Position(sel.Sel.End()), selection.Obj())
	}
}

func addRef(fset *token.FileSet, fileIdx map[string]uint32, enc *objectpath.Encoder, b *store.Builder, idx *store.PackageIndexEntries, pos, end token.Position, obj types.Object) {
	if obj == nil || isSkippedObject(obj) {
		return
	}
	fi, ok := fileIdx[pos.Filename]
	if !ok {
		return
	}
	sid := symbolID(obj, enc, fset)
	idHash := store.Hash(sid)
	b.AddRef(store.RefInput{
		FileIdx:        fi,
		Line:           u32pos(pos.Line),
		Col:            u32pos(pos.Column),
		EndCol:         u32pos(end.Column),
		ToSymbolIDHash: idHash,
		ToPkgHash:      store.Hash(obj.Pkg().Path()),
	})
	idx.SymStrs = append(idx.SymStrs, store.SymStrEntry{IDHash: idHash, SymbolID: sid})
}

// registerMethodSet records every method in named's pointer method set (a
// superset of its value method set) under idx's method index, keyed by
// method name, so implementation queries can find named as a candidate
// receiver type via a name-based first pass.
func registerMethodSet(idx *store.PackageIndexEntries, pkgHash, typeIDHash uint64, named *types.Named) {
	ms := types.NewMethodSet(types.NewPointer(named))
	for i := 0; i < ms.Len(); i++ {
		fn, ok := ms.At(i).Obj().(*types.Func)
		if !ok {
			continue
		}
		idx.Methods = append(idx.Methods, store.MethodSymbolEntry{
			Name:  fn.Name(),
			Entry: store.MethodEntry{PkgHash: pkgHash, TypeSymbolIDHash: typeIDHash},
		})
	}
}

// registerInterfaceMethodSet records every method named declares directly
// (not those promoted from embedded interfaces beyond what NumMethods/Method
// already flatten) under idx's method index, alongside registerMethodSet's
// concrete-type entries. An implementation query distinguishes the two by
// looking up each candidate's [store.Symbol.Kind]: this lets both directions
// of an implementation query (interface -> implementers, concrete type ->
// interfaces it satisfies) share the same name-based first pass.
func registerInterfaceMethodSet(idx *store.PackageIndexEntries, pkgHash, typeIDHash uint64, named *types.Named) {
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return
	}
	for i := 0; i < iface.NumMethods(); i++ {
		idx.Methods = append(idx.Methods, store.MethodSymbolEntry{
			Name:  iface.Method(i).Name(),
			Entry: store.MethodEntry{PkgHash: pkgHash, TypeSymbolIDHash: typeIDHash},
		})
	}
}

// isSkippedObject reports whether obj denotes something facts extraction
// has no use for: an import name, a predeclared identifier, or an object
// with no home package (predeclared error/any/nil and similar).
func isSkippedObject(obj types.Object) bool {
	switch obj.(type) {
	case *types.PkgName, *types.Builtin, *types.Nil:
		return true
	}
	return obj.Pkg() == nil
}

// symbolID returns the canonical SymbolID string for obj: its defining
// package's import path plus an objectpath (stable across separately
// type-checked copies of the same package, so a reference computed while
// checking one package matches the SymbolID computed while checking obj's
// defining package). Objects unreachable from their package scope (e.g.
// function-local variables) fall back to a position-based path.
func symbolID(obj types.Object, enc *objectpath.Encoder, fset *token.FileSet) string {
	pkgPath := obj.Pkg().Path()
	if path, err := enc.For(obj); err == nil {
		return store.BuildSymbolID(pkgPath, string(path))
	}
	pos := fset.Position(obj.Pos())
	return store.BuildSymbolID(pkgPath, fmt.Sprintf("%s:%d:%d", pos.Filename, pos.Line, pos.Column))
}

// classify maps a types.Object to its store Kind byte plus flags.
func classify(obj types.Object) (kind, flags uint8) {
	if obj.Exported() {
		flags |= FlagExported
	}
	switch o := obj.(type) {
	case *types.Func:
		if o.Signature().Recv() != nil {
			return KindMethod, flags
		}
		return KindFunc, flags
	case *types.TypeName:
		if types.IsInterface(o.Type()) {
			return KindInterface, flags
		}
		return KindType, flags
	case *types.Var:
		if o.IsField() {
			return KindField, flags
		}
		return KindVar, flags
	case *types.Const:
		return KindConst, flags
	default:
		return KindVar, flags
	}
}

// qualifier returns a types.Qualifier that renders identifiers from pkg
// unqualified and every other package by its short name, for
// [store.SymbolInput.Sig].
func qualifier(pkg *types.Package) types.Qualifier {
	return func(p *types.Package) string {
		if p == pkg {
			return ""
		}
		return p.Name()
	}
}

// collectDocsInto records, for every identifier in f that declares a
// documented func, type, var/const, struct field, or interface method, the
// Doc.Text() of its associated comment.
func collectDocsInto(f *ast.File, docs map[*ast.Ident]string) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			recordDoc(docs, d.Doc, identSlice(d.Name))
		case *ast.GenDecl:
			collectGenDeclDocs(d, docs)
		case *ast.Field:
			recordDoc(docs, d.Doc, d.Names)
		}
		return true
	})
}

// collectGenDeclDocs handles type/var/const declarations: a spec's own Doc
// takes priority, falling back to the declaration's Doc when it is the only
// spec in the group (the non-parenthesized `type T ...` / `var x ...` case).
func collectGenDeclDocs(d *ast.GenDecl, docs map[*ast.Ident]string) {
	single := len(d.Specs) == 1
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			recordDoc(docs, specDoc(s.Doc, d.Doc, single), identSlice(s.Name))
		case *ast.ValueSpec:
			recordDoc(docs, specDoc(s.Doc, d.Doc, single), s.Names)
		}
	}
}

func specDoc(own, parent *ast.CommentGroup, singleSpec bool) *ast.CommentGroup {
	if own != nil || !singleSpec {
		return own
	}
	return parent
}

func recordDoc(docs map[*ast.Ident]string, doc *ast.CommentGroup, names []*ast.Ident) {
	if doc == nil {
		return
	}
	text := doc.Text()
	for _, name := range names {
		if name != nil {
			docs[name] = text
		}
	}
}

func identSlice(id *ast.Ident) []*ast.Ident {
	if id == nil {
		return nil
	}
	return []*ast.Ident{id}
}
