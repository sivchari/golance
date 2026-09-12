package xref

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
	"golang.org/x/tools/go/ast/astutil"
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
	_, _, loc, err := r.symbolByHash(ctx, target.PkgHash, target.IDHash)
	if err != nil {
		if errors.Is(err, errSymbolNotFound) {
			return nil, fmt.Errorf("xref: definition of %s not found in its own package facts", target.Name)
		}
		return nil, err
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
		_, _, loc, err := r.symbolByHash(ctx, target.PkgHash, target.IDHash)
		if err != nil {
			if errors.Is(err, errSymbolNotFound) {
				return nil, fmt.Errorf("xref: definition of %s not found in its own package facts", target.Name)
			}
			return nil, err
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

// locationsForAll collects every location referencing any symbol in wanted,
// via one [store.DB.PostingsFor] prefix scan per wanted symbol: each scan
// costs work proportional to that symbol's own result size (how many
// packages reference it, and how many locations each contributes), not to
// the number of packages that COULD reference it — the reverse reference
// index (internal/store's bucketRefPostings) is keyed by (targetPkgHash,
// targetIDHash, srcPkgHash) precisely so this lookup never has to fall back
// to scanning every unit in wanted's defining package's reverse-dependency
// closure the way locationsFor's predecessor did.
//
// A single physical reference location can only ever be posted under ONE
// wanted symbol's key (store.Ref/store.PostingEntry both carry exactly one
// (ToPkgHash, ToSymbolIDHash) target), so unioning every wanted symbol's own
// results here can never introduce a duplicate the way merging overlapping
// closure walks used to risk — no additional internal dedup is needed
// beyond References' own top-level dedupeLocations, which exists for a
// different reason (folding in the declaration location).
//
// ctx is checked once per wanted symbol (rather than per posting record):
// wanted is always small (References' own target plus, for a method, its
// corresponding-method symbols — see References' doc), so this is a
// lighter-weight cancellation point than the old per-closure-unit check,
// not a coarser one.
func (r *Resolver) locationsForAll(ctx context.Context, wanted []resolvedSymbol) ([]Location, error) {
	var out []Location
	for _, w := range wanted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		locs, err := r.postingsFor(ctx, w.PkgHash, w.IDHash)
		if err != nil {
			return nil, err
		}
		out = append(out, locs...)
	}

	sortLocations(out)
	return out, nil
}

// postingsFor returns every location referencing the symbol identified by
// (targetPkgHash, targetIDHash), read directly from [store.DB]'s postings
// index rather than any cached facts blob (see store.DB.PostingsFor's doc):
// a posting is written in the same bbolt transaction as its source
// package's own unit-pointer swap, so this always sees whatever a query's
// own db.GetUnit call would too, with no separate cache to invalidate.
// Reports one AddUnit call per distinct source package's posting record to
// ctx's installed StatsSink -- bytesRead/recordsScanned now mean the
// posting record's own encoded size and location count, the direct
// counterpart of what scanUnitForWanted's predecessor reported per closure
// unit, just at the new path's actual granularity.
func (r *Resolver) postingsFor(ctx context.Context, targetPkgHash, targetIDHash uint64) ([]Location, error) {
	recs, err := r.db.PostingsFor(ctx, targetPkgHash, targetIDHash)
	if err != nil {
		return nil, err
	}
	var out []Location
	for _, rec := range recs {
		for _, loc := range rec.Locations {
			out = append(out, Location{File: absPath(r.root, loc.File, r.relative), Line: loc.Line, Col: loc.Col, EndCol: loc.EndCol})
		}
		addUnit(ctx, rec.Bytes, len(rec.Locations))
	}
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
		name, kind, loc, err := r.symbolByHash(ctx, store.Hash(pkgPath), idHash)
		if err != nil {
			if errors.Is(err, errSymbolNotFound) {
				continue
			}
			return SymbolInfo{}, false, err
		}
		return SymbolInfo{Name: name, Kind: kind, Container: pkgPath, Location: loc}, true, nil
	}
	return SymbolInfo{}, false, nil
}

// Rename returns the edits needed to rename the symbol at (file, line, col)
// to newName, grouped by file. It includes every reference across the
// defining package's reverse-dependency closure plus the declaration itself.
//
// When target is a type, renaming it also renames every promoted-field use
// its own name implies: embedding a type in a struct makes the type's name
// double as that field's implicit name (e.g. `Container{Box: ...}`), so a
// composite-literal key or selector expression using that promoted name
// must be rewritten too, even though it resolves to a distinct *types.Var
// field object rather than to target's own *types.TypeName -- see
// embeddedFieldSymbols.
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

	_, _, declLoc, err := r.symbolByHash(ctx, target.PkgHash, target.IDHash)
	if err != nil {
		if errors.Is(err, errSymbolNotFound) {
			return nil, fmt.Errorf("xref: definition of %s not found in its own package facts", target.Name)
		}
		return nil, err
	}
	refs, err := r.locationsForAll(ctx, []resolvedSymbol{target})
	if err != nil {
		return nil, err
	}
	locs := append([]Location{declLoc}, refs...)

	if target.Kind == index.KindType || target.Kind == index.KindInterface {
		fields, err := r.embeddedFieldSymbols(ctx, refs)
		if err != nil {
			return nil, err
		}
		if len(fields) > 0 {
			fieldRefs, err := r.locationsForAll(ctx, fields)
			if err != nil {
				return nil, err
			}
			locs = append(locs, fieldRefs...)
		}
	}

	locs = dedupeLocations(locs)
	sortLocations(locs)

	edits := make(map[string][]Edit)
	for _, loc := range locs {
		edits[loc.File] = append(edits[loc.File], Edit{Line: loc.Line, Col: loc.Col, EndCol: loc.EndCol, NewText: newName})
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	docEdits, err := docCommentEdits(declLoc.File, declLoc.Line, declLoc.Col, target.Name, newName)
	if err != nil {
		return nil, err
	}
	edits[declLoc.File] = append(edits[declLoc.File], docEdits...)

	return edits, nil
}

// docCommentEdits returns the edits needed to rewrite every whole-word
// occurrence of oldName within the doc comment gopls v0.23.0's own renamer
// (internal/golang/rename.go's docComment, in golang.org/x/tools/gopls)
// would rewrite for the declaration at (file, declLine, declCol) -- not a
// "the comment starts with the name" prefix check, despite that being the
// documented Go convention a doc comment follows: gopls instead walks up
// from the declaring identifier to the nearest enclosing *ast.FuncDecl,
// *ast.Field, *ast.GenDecl, or a *ast.TypeSpec/*ast.ValueSpec that carries
// its own Doc (see findDocOwner), and, once such a CommentGroup is found,
// regexp-replaces every \bOLDNAME\b match anywhere within it -- including a
// later line of a multi-line comment, and every repeated mention on one
// line -- not merely a leading occurrence. A comment belonging to a
// different declaration that merely mentions the same text is never
// touched, because the walk only ever starts from oldName's own declaring
// identifier. Returns (nil, nil) when that declaration has no such comment
// to rewrite.
//
// Deliberate divergence: gopls's own rename separately rewrites doc-link
// references elsewhere in the package, e.g. "[pkg.Box]" in an unrelated
// declaration's comment (see gopls's updateCommentDocLinks). This function
// only ever touches the renamed declaration's OWN doc comment, matching
// this package's existing "edit whatever the type-checked facts index
// resolves, not free text elsewhere" scope for Rename (see the func's own
// doc); closing that separate gap is future work, not part of this fix.
func docCommentEdits(file string, declLine, declCol uint32, oldName, newName string) ([]Edit, error) {
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return nil, fmt.Errorf("xref: read %s for doc comment rename: %w", file, err)
	}
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, file, data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("xref: parse %s for doc comment rename: %w", file, err)
	}

	id := identAt(fset, astFile, declLine, declCol, oldName)
	if id == nil {
		return nil, nil
	}
	path, _ := astutil.PathEnclosingInterval(astFile, id.Pos(), id.End())
	doc := findDocOwner(fset, astFile, id, path)
	if doc == nil {
		return nil, nil
	}

	tf := fset.File(astFile.Pos())
	nameRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(oldName) + `\b`)
	var edits []Edit
	for _, c := range doc.List {
		if isDirectiveComment(c.Text) {
			continue
		}
		// go/parser strips \r from Comment.Text, so a later line's start is
		// looked up from tf's own line table (tf.LineStart) rather than
		// derived by summing stripped-line lengths, keeping byte offsets
		// correct even when the source uses CRLF line endings.
		lines := strings.Split(c.Text, "\n")
		firstLine := tf.Line(c.Pos())
		for i, line := range lines {
			lineStart := c.Pos()
			if i > 0 {
				lineStart = tf.LineStart(firstLine + i)
			}
			for _, m := range nameRe.FindAllStringIndex(line, -1) {
				start := tf.Position(lineStart + token.Pos(m[0]))
				end := tf.Position(lineStart + token.Pos(m[1]))
				edits = append(edits, Edit{
					Line:    u32pos(start.Line),
					Col:     u32pos(start.Column),
					EndCol:  u32pos(end.Column),
					NewText: newName,
				})
			}
		}
	}
	return edits, nil
}

// u32pos converts a go/token.Position's Line or Column (always positive
// for a position go/parser itself produced) to uint32, panicking on the
// same "this should be structurally impossible" grounds as resolver.go's
// u32len.
func u32pos(n int) uint32 {
	if n < 0 || n > math.MaxUint32 {
		panic(fmt.Sprintf("xref: position %d out of uint32 range", n))
	}
	return uint32(n)
}

// identAt returns the *ast.Ident named name positioned at exactly (line,
// col) in astFile, or nil if none does -- there is always at most one,
// since two identifiers can never start at the same byte position.
func identAt(fset *token.FileSet, astFile *ast.File, line, col uint32, name string) *ast.Ident {
	var found *ast.Ident
	ast.Inspect(astFile, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		pos := fset.Position(id.Pos())
		if u32pos(pos.Line) == line && u32pos(pos.Column) == col {
			found = id
		}
		return true
	})
	return found
}

// findDocOwner returns the doc comment gopls's own renamer would rewrite
// for id, whose ancestor chain -- innermost (id itself) first -- is path,
// as returned by [astutil.PathEnclosingInterval]. Mirrors gopls's
// internal/golang.docComment: the first *ast.FuncDecl, *ast.Field, or
// *ast.GenDecl found while walking outward owns the comment; a
// *ast.TypeSpec or *ast.ValueSpec only owns it if it carries its own Doc
// (a per-spec comment inside a grouped declaration), otherwise the walk
// continues past it to its own enclosing GenDecl; any other node kind
// means there is no doc comment, with one exception -- gopls also treats a
// comment immediately above a "name := expr" statement as that
// declaration's doc, since ":=" has no Doc field of its own to carry one
// (see precedingComment).
func findDocOwner(fset *token.FileSet, astFile *ast.File, id *ast.Ident, path []ast.Node) *ast.CommentGroup {
	for _, n := range path {
		switch decl := n.(type) {
		case *ast.FuncDecl:
			return decl.Doc
		case *ast.Field:
			return decl.Doc
		case *ast.GenDecl:
			return decl.Doc
		case *ast.TypeSpec:
			if decl.Doc != nil {
				return decl.Doc
			}
		case *ast.ValueSpec:
			if decl.Doc != nil {
				return decl.Doc
			}
		case *ast.Ident:
			// id itself, the walk's own starting point; keep going outward.
		case *ast.AssignStmt:
			if decl.Tok != token.DEFINE {
				return nil
			}
			return precedingComment(fset, astFile, id)
		default:
			return nil
		}
	}
	return nil
}

// precedingComment returns the comment group ending on the line
// immediately above id's own line, the ":=" doc-comment convention
// findDocOwner's *ast.AssignStmt case implements.
func precedingComment(fset *token.FileSet, astFile *ast.File, id *ast.Ident) *ast.CommentGroup {
	identLine := fset.Position(id.Pos()).Line
	for _, c := range astFile.Comments {
		if c.Pos() > id.Pos() {
			continue
		}
		if fset.Position(c.End()).Line+1 == identLine {
			return c
		}
	}
	return nil
}

// isDirectiveComment reports whether text -- a single "//"-style comment
// line's full text, including its leading "//" -- is a compiler or tool
// directive (e.g. "//go:generate", "//line file:1") rather than doc prose,
// using the same "//line " / "//[a-z0-9]+:[a-z0-9]" shape go/printer and
// gopls itself both recognize a directive by. Rename must skip these:
// rewriting inside one would corrupt the directive instead of renaming a
// mention of oldName, and a directive is never doc prose in the first
// place.
func isDirectiveComment(text string) bool {
	if len(text) < 3 || text[1] != '/' {
		return false
	}
	c := text[2:]
	if c == "" {
		return false
	}
	if strings.HasPrefix(c, "line ") {
		return true
	}
	colon := strings.IndexByte(c, ':')
	if colon <= 0 || colon+1 >= len(c) {
		return false
	}
	for i := 0; i <= colon+1; i++ {
		if i == colon {
			continue
		}
		b := c[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}

// embeddedFieldSymbols returns the resolvedSymbol for the promoted field
// implicitly declared at each of locs that names an anonymous struct field
// -- one entry per struct that embeds the renamed type. An embedded type's
// name doubles as that field's own name, so go/types records the same
// identifier both as a use of the type (already among the renamed type's
// own references, hence present in locs) and as the definition of a
// distinct *types.Var field; the latter is what a composite-literal key or
// selector expression using the promoted name actually resolves to, and it
// is never itself among the type's own references (see the doc on Rename).
// Only KindType/KindInterface renames call this, since only a type name can
// be embedded as an anonymous field.
func (r *Resolver) embeddedFieldSymbols(ctx context.Context, locs []Location) ([]resolvedSymbol, error) {
	var out []resolvedSymbol
	for _, loc := range locs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pkgPath, ok := r.pkgPathForFile(loc.File)
		if !ok {
			continue
		}
		pkgHash := store.Hash(pkgPath)
		u, err := r.unitBlob(ctx, pkgHash)
		if err != nil {
			continue
		}
		v, err := store.NewView(u.Facts)
		if err != nil {
			continue
		}
		fileIdx, ok := r.fileIndexOf(v, loc.File)
		if !ok {
			continue
		}
		s, ok := symbolAtPosition(v, fileIdx, loc.Line, loc.Col)
		if !ok || s.Kind() != index.KindField {
			continue
		}
		out = append(out, resolvedSymbol{PkgHash: pkgHash, IDHash: s.IDHash(), Kind: s.Kind(), Name: s.Name()})
	}
	return out, nil
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
