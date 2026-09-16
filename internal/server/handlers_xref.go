package server

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"golang.org/x/tools/go/ast/astutil"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// resolverOrWarn returns the current facts-index Resolver, or ok=false if
// the indexer subprocess has not completed a build yet. On the first such
// call it logs the reason to the client once via window/logMessage (not
// showMessage: the index building is routine, not a failure, and some
// clients render showMessage as a blocking modal — see logMessage's doc);
// later calls stay silent so a burst of queries during index build does not
// spam the log. handleDefinition is the one caller that still answers an
// ok=false result via its own fallback chain (definitionFallback, which
// serves useful results without the index at all) rather than an error;
// every other cross-reference handler
// (references/implementation/workspaceSymbol/rename) answers with
// indexUnavailableError instead of an ordinary empty result — see its own
// doc for why an empty result is not safe here even though $/progress (see
// relayIndexProgress) also tells the user a build is under way.
func (s *Server) resolverOrWarn() (*xref.Resolver, bool) {
	idx := s.idx.Load()
	if idx == nil {
		if s.indexBuildingWarned.CompareAndSwap(false, true) {
			s.logMessage("golance: the workspace index is still building; cross-reference results are unavailable until it completes")
		}
		return nil, false
	}
	return idx.resolver, true
}

// indexUnavailableError is the LSP error references/implementation/
// workspaceSymbol/rename answer with while resolverOrWarn reports ok=false,
// instead of an ordinary empty result: an automated client (e.g. an AI
// coding agent) cannot otherwise tell "0 matches because the index has not
// finished building yet" apart from a genuine "0 matches, this symbol is
// really unused" — a real field report traced exactly that misread back to
// an empty references result returned during index build. Blocking the
// request instead, the way gopls generally waits for its own snapshot to
// become ready, is not acceptable here since a cold-start build can take
// minutes; LSPErrorCodesRequestFailed (a request that was syntactically
// fine but cannot currently be answered) makes the two states
// machine-distinguishable without making the client wait.
//
// The message itself distinguishes a build genuinely in flight from one
// that already failed (s.indexFailedWarned — set only when a build attempt
// finished leaving no index open at all, see warnIndexUnavailable's
// callers): claiming "still building" in the failed case would be
// inaccurate and mislead a caller into thinking a retry will eventually
// succeed on its own. feature names the specific request kind (e.g.
// "references") for the message.
func (s *Server) indexUnavailableError(feature string) error {
	if s.indexFailedWarned.Load() {
		return rpc.NewError(int32(protocol.LSPErrorCodesRequestFailed), fmt.Sprintf("golance: the workspace index failed to build; %s is unavailable until the next successful build", feature))
	}
	return rpc.NewError(int32(protocol.LSPErrorCodesRequestFailed), fmt.Sprintf("golance: the workspace index is still building; %s is unavailable until it completes", feature))
}

// xrefPosition converts an LSP Position for path's current editor buffer
// into the 1-based line/byte-column coordinates internal/xref queries
// take, correcting for any unsaved edits (see dirty.go) since xref answers
// from the on-disk facts index.
//
// ok is also false, silently, when pos falls outside text's current bounds
// (positionToXref): a narrow race between the client's query and a
// just-applied edit shrinking the file, which the client's very next
// request already answers against the up-to-date buffer -- logging that
// would be noise for something that is never actionable. An overlay read
// failure is a different matter (see below) and is logged.
func (s *Server) xrefPosition(path string, pos protocol.Position) (line, col int, ok bool) {
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		s.logger.Printf("server: xref position for %s: %v", path, err)
		return 0, 0, false
	}
	l, c, ok := positionToXref(text, pos)
	if !ok {
		return 0, 0, false
	}
	if l < 0 {
		l = 0
	}
	if l > math.MaxUint32 {
		l = math.MaxUint32
	}
	return int(s.correctQueryLine(path, uint32(l))), c, true
}

// toLSPLocations converts xref Locations to LSP Locations, applying dirty
// correction per result file and dropping any that cannot be resolved.
func (s *Server) toLSPLocations(locs []xref.Location) protocol.LocationSlice {
	out := make(protocol.LocationSlice, 0, len(locs))
	for _, loc := range locs {
		if pl, ok := s.correctResultLocation(loc); ok {
			out = append(out, pl)
		}
	}
	return out
}

func (s *Server) correctResultLocation(loc xref.Location) (protocol.Location, bool) {
	rng, ok := s.correctResultRange(loc.File, loc.Line, loc.Col, loc.EndCol)
	if !ok {
		return protocol.Location{}, false
	}
	return protocol.Location{URI: uri.File(loc.File), Range: rng}, true
}

func (s *Server) handleDefinition(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.DefinitionParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return s.definitionFallback(ctx, p.TextDocument.URI, p.Position), nil
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	phaseTimerFrom(ctx).enter("facts.Definition")
	locs, err := resolver.Definition(ctx, path, line, col)
	if err != nil {
		// Most errors here mean "no symbol at this position" — a routine
		// outcome the LSP client already handles as an empty result, not
		// something to surface as a protocol error. Log it so a genuine
		// facts-read failure is still visible, rather than silently
		// indistinguishable from an ordinary miss.
		s.logger.Printf("server: definition at %s:%d:%d: %v", path, line, col, err)
		return s.definitionFallback(ctx, p.TextDocument.URI, p.Position), nil
	}
	if len(locs) == 0 {
		// The facts index only ever covers root (workspace) packages (see
		// internal/index/scheduler.go's doc) and can otherwise legitimately
		// have no entry for a resolvable position; fall through to the
		// same type-info-based path used when the index cannot be
		// consulted at all, rather than treating an empty index answer as
		// final.
		return s.definitionFallback(ctx, p.TextDocument.URI, p.Position), nil
	}
	return s.toLSPLocations(locs), nil
}

// definitionFallback answers handleDefinition entirely from the
// type-checked package's own AST/types.Info/FileSet, for whenever the
// workspace facts index cannot answer: it has not finished building yet
// (resolverOrWarn's ok=false), a store query against it failed, or it
// legitimately has no entry for this position. A package-QUALIFIER
// identifier (the "os" in "os.Getenv") resolves via
// langfeat.PackageNameDefinition to its import spec, tried before
// SamePackageDefinition since a *types.PkgName's Pkg() reports the
// importing package and would otherwise be mistaken for one (see
// PackageNameDefinition's doc). An identifier declared in cp's own package
// resolves via langfeat.SamePackageDefinition, exact down to the column,
// needing no index at all; a standard library or module dependency
// identifier resolves through dependencyDefinition's depcheck.Provider path
// instead, exact to the column as well (see internal/depcheck's package
// doc). A different *workspace* (root) package's identifier is deliberately
// left unanswered here — see dependencyDefinition's doc for why.
func (s *Server) definitionFallback(ctx context.Context, u uri.URI, pos protocol.Position) protocol.LocationSlice {
	cf := s.checkedFile(ctx, u, pos)
	if !cf.ok {
		return nil
	}
	if loc, ok := s.importDefinition(cf); ok {
		return s.toLSPLocations([]xref.Location{loc})
	}
	if loc, ok := s.builtinDefinition(cf); ok {
		return s.toLSPLocations([]xref.Location{loc})
	}
	if info, err := langfeat.PackageNameDefinition(cf.cp, cf.path, cf.offset); err != nil {
		s.logger.Printf("server: package name definition %s: %v", cf.path, err)
	} else if info != nil {
		return s.samePackageDefinitionLocation(info)
	}
	info, err := langfeat.SamePackageDefinition(cf.cp, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: same-package definition %s: %v", cf.path, err)
	} else if info != nil {
		return s.samePackageDefinitionLocation(info)
	}
	if loc, ok := s.dependencyDefinition(ctx, cf); ok {
		return s.toLSPLocations([]xref.Location{loc})
	}
	return nil
}

// samePackageDefinitionLocation converts a SamePackageDefInfo (byte offsets
// against info.File's own current buffer) into an LSP location, the same
// pattern typeDefinitionSameFile uses for langfeat.TypeDefInfo.
func (s *Server) samePackageDefinitionLocation(info *langfeat.SamePackageDefInfo) protocol.LocationSlice {
	text, err := s.overlay.ReadFile(info.File)
	if err != nil {
		s.logger.Printf("server: same-package definition read %s: %v", info.File, err)
		return nil
	}
	rng, ok := offsetRangeToLSP(text, info.Range.StartOffset, info.Range.EndOffset)
	if !ok {
		return nil
	}
	return protocol.LocationSlice{{URI: uri.File(info.File), Range: rng}}
}

// builtinDefinition is definitionFallback's path for a universe
// (predeclared) identifier or the error interface's Error method — Pkg()
// == nil, unambiguous from the identifier's own resolved types.Object, so
// this runs before SamePackageDefinition/dependencyDefinition's own
// (redundant, since both decline Pkg() == nil already) resolution attempt
// — resolved into the toolchain's $GOROOT/src/builtin/builtin.go, gopls's
// own resolution target for these same identifiers (see
// langfeat.BuiltinDefinition's doc).
func (s *Server) builtinDefinition(cf checkedFileResult) (xref.Location, bool) {
	info, err := langfeat.BuiltinDefinition(cf.cp, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: builtin definition %s: %v", cf.path, err)
		return xref.Location{}, false
	}
	if info == nil {
		return xref.Location{}, false
	}
	return builtinDefLocation(s.logger, info)
}

// builtinDefLocation converts a BuiltinDefInfo -- builtinDefinition's own
// (langfeat.BuiltinDefinition) or handleTypeDefinition's (a predeclared
// named type's TypeDefInfo.Builtin, langfeat.TypeDefinition) -- into an
// xref.Location. ok is false if builtin.go no longer exists on disk (a
// relocated or removed toolchain install between resolution and this
// call, logged since every predeclared identifier's hover/definition/
// typeDefinition silently degrades for the rest of the process once this
// starts happening — see loadBuiltinFileOnce's own process-lifetime cache)
// or any coordinate overflows uint32, the same bounds check every other
// xref.Location construction in this package applies.
func builtinDefLocation(logger *log.Logger, info *langfeat.BuiltinDefInfo) (xref.Location, bool) {
	if _, err := os.Stat(info.Filename); err != nil {
		logger.Printf("server: builtin definition: declaration source %s: %v", info.Filename, err)
		return xref.Location{}, false
	}
	if info.Line <= 0 || int64(info.Line) > math.MaxUint32 ||
		info.Col <= 0 || int64(info.Col) > math.MaxUint32 ||
		info.EndCol <= 0 || int64(info.EndCol) > math.MaxUint32 {
		return xref.Location{}, false
	}
	return xref.Location{File: info.Filename, Line: uint32(info.Line), Col: uint32(info.Col), EndCol: uint32(info.EndCol)}, true
}

// dependencyDefinition is definitionFallback's path for a symbol not
// declared in cf's own package and not yet (or not ever) answerable from
// the workspace facts index: the standard library, a module dependency, or
// another workspace (root) package while the facts index is still building
// or legitimately has no entry for this position (see
// internal/index/scheduler.go's doc — an individual package can end up with
// none). This resolves it instead through the type-checked package's own
// Uses/Defs, mapped into a source-type-checked copy of the target package
// via ws.depProvider (internal/depcheck) — exact to the column, and able to
// see unexported types (see internal/langfeat.DependencyDefinition,
// internal/depcheck's package doc) — rather than the line-only,
// exported-only positions gcexportdata/depCache offer.
//
// A root-package target used to be excluded here entirely, to avoid a
// stale-data race TestE2E_WorktreeSharesIndex once pinned: a save landing
// while the facts index was still unavailable used to be silently dropped,
// with no retry once the index later opened, so an early answer risked
// masking that loss. handleDidSave's markDirty/drainDirty (documentsync.go)
// closed that gap — a save made during the index-build window is always
// reindexed once the index opens, whether or not this path already
// answered a query for it — so the exclusion no longer protects against
// anything, and cost every cross-package "go to definition" the whole
// cold-start index build's duration (minutes, on a large workspace) for no
// benefit: a healthy facts index still always wins once built, since
// handleDefinition consults it before ever reaching this fallback.
//
// Before falling all the way to depcheck.Decl's source-check, this first
// tries resolving the same target through the on-disk facts index (an O(1)
// DB read) via facts-index-declaration below: the identical fast path
// typeDefinitionCrossPackage already takes for textDocument/typeDefinition.
// This matters most for a ROOT package target the facts index actually has
// an entry for but resolveAt's own reverse-postings lookup missed (a stale
// or as-yet-unindexed reference) — depcheck.Decl would otherwise
// source-type-check that whole root package from scratch just to answer a
// query the index could resolve in a DB read, the dominant cost behind a
// slow "go to definition" on a large workspace.
func (s *Server) dependencyDefinition(ctx context.Context, cf checkedFileResult) (xref.Location, bool) {
	ws := s.workspace()
	if ws == nil {
		return xref.Location{}, false
	}
	if loc, ok := s.factsIndexDeclaration(ctx, cf); ok {
		return loc, true
	}
	phaseTimerFrom(ctx).enter("depcheck.Decl")
	info, err := langfeat.DependencyDefinition(ctx, cf.cp, ws.depProvider, cf.path, cf.offset)
	if err != nil {
		s.logger.Printf("server: dependency definition %s: %v", cf.path, err)
		return xref.Location{}, false
	}
	if info == nil {
		return xref.Location{}, false
	}
	if _, err := os.Stat(info.Filename); err != nil {
		s.logger.Printf("server: dependency definition %s: declaration source %s: %v", cf.path, info.Filename, err)
		return xref.Location{}, false
	}
	if info.Line <= 0 || int64(info.Line) > math.MaxUint32 ||
		info.Col <= 0 || int64(info.Col) > math.MaxUint32 ||
		info.EndCol <= 0 || int64(info.EndCol) > math.MaxUint32 {
		return xref.Location{}, false
	}
	return xref.Location{File: info.Filename, Line: uint32(info.Line), Col: uint32(info.Col), EndCol: uint32(info.EndCol)}, true
}

// factsIndexDeclaration is dependencyDefinition's fast pre-check: it
// resolves cf's cursor identifier to (package path, objectpath) via
// langfeat.DependencyDefinitionTarget — the same identity computation
// internal/index's facts extraction uses — and looks that up directly in
// the on-disk facts index via Resolver.TypeDeclaration, an O(1) DB read.
// ok is false whenever that lookup cannot answer (the resolver is not yet
// available, the identifier is not a dependency reference, or the target's
// package has no facts recorded), leaving dependencyDefinition to fall back
// to depcheck.Decl exactly as before.
func (s *Server) factsIndexDeclaration(ctx context.Context, cf checkedFileResult) (xref.Location, bool) {
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return xref.Location{}, false
	}
	pkgPath, objPath, ok := langfeat.DependencyDefinitionTarget(cf.cp, cf.path, cf.offset)
	if !ok {
		return xref.Location{}, false
	}
	phaseTimerFrom(ctx).enter("facts.Decl")
	return resolver.TypeDeclaration(ctx, pkgPath, objPath)
}

// importDefinition is definitionFallback's path for the cursor being inside
// an import spec's path string (e.g. the quoted "encoding/json"): facts
// extraction never indexes an *ast.ImportSpec (see
// langfeat.ImportPathDefinition's doc), and neither SamePackageDefinition
// nor DependencyDefinition finds an *ast.Ident there to resolve, so without
// this the query answers nothing at all -- the gap this exists to close.
//
// Per gopls, "Go to Definition" on an import path jumps into the imported
// package. internal/graph's Snapshot already has every package in the
// workspace's transitive import graph -- root, module dependency, AND
// standard library alike, since internal/graph's loadMode requests
// NeedFiles for the whole closure, not just root packages -- with real,
// on-disk Go files, so (unlike documentLinkTarget in handlers_nav.go, which
// substitutes a pkg.go.dev URL for anything outside the workspace)
// resolving an import path here never needs that same distinction: any
// package the graph loaded degrades only when it has no Go files
// (unsafe/builtin, or a load failure), not by origin.
//
// gopls itself returns one location per file of the resolved package (see
// golang.org/x/tools/gopls's importDefinition); this instead points at just
// the package's first Go file's package-clause identifier -- the same
// single-file simplification handlers_codeaction.go's packageNameOf
// already makes for the identical "read a package's own declared name"
// need. A definition result needs one always-present, unambiguous
// location, and this still lands the cursor inside the target package,
// without a query-time parse of every one of its files for marginal
// benefit.
func (s *Server) importDefinition(cf checkedFileResult) (xref.Location, bool) {
	pkgPath, ok := langfeat.ImportPathDefinition(cf.cp, cf.path, cf.offset)
	if !ok {
		return xref.Location{}, false
	}
	ws := s.workspace()
	if ws == nil {
		return xref.Location{}, false
	}
	pkg, ok := ws.snap.Package(pkgPath)
	if !ok || len(pkg.GoFiles) == 0 {
		return xref.Location{}, false
	}
	return packageClauseLocation(pkg.GoFiles[0])
}

// packageClauseLocation parses file's package clause fresh from disk (like
// packageNameOf in handlers_codeaction.go) and returns a Location for its
// package name identifier -- e.g. the "io" in "package io" -- rather than
// the bare "package" keyword, consistent with every other Location this
// package returns pointing at a name span, not a keyword.
func packageClauseLocation(file string) (xref.Location, bool) {
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, file, nil, parser.PackageClauseOnly)
	if err != nil || astFile.Name == nil {
		return xref.Location{}, false
	}
	start := fset.Position(astFile.Name.Pos())
	end := fset.Position(astFile.Name.End())
	if start.Line <= 0 || int64(start.Line) > math.MaxUint32 ||
		start.Column <= 0 || int64(start.Column) > math.MaxUint32 ||
		end.Column <= 0 || int64(end.Column) > math.MaxUint32 {
		return xref.Location{}, false
	}
	return xref.Location{
		File:   file,
		Line:   uint32(start.Line),
		Col:    uint32(start.Column),
		EndCol: uint32(end.Column),
	}, true
}

func (s *Server) handleReferences(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.ReferenceParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		if locs, ok := s.packageNameReferencesFallback(ctx, p.TextDocument.URI, p.Position, p.Context.IncludeDeclaration); ok {
			return locs, nil
		}
		return nil, s.indexUnavailableError("references")
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	if pt := phaseTimerFrom(ctx); pt != nil {
		ctx = xref.WithStatsSink(ctx, pt)
	}
	locs, err := resolver.References(ctx, path, line, col, p.Context.IncludeDeclaration)
	if err != nil {
		// See handleDefinition's comment: most errors here are an ordinary
		// "no symbol at this position" miss, but log it anyway so a
		// genuine facts-read failure does not vanish silently. A
		// package-qualifier identifier is exactly such a miss -- the facts
		// index never records a *types.PkgName as an indexable symbol (see
		// packageNameReferencesFallback's doc) -- so try that fallback
		// before giving up.
		s.logger.Printf("server: references at %s:%d:%d: %v", path, line, col, err)
		if locs, ok := s.packageNameReferencesFallback(ctx, p.TextDocument.URI, p.Position, p.Context.IncludeDeclaration); ok {
			return locs, nil
		}
		return protocol.LocationSlice(nil), nil
	}
	if len(locs) == 0 {
		if fb, ok := s.packageNameReferencesFallback(ctx, p.TextDocument.URI, p.Position, p.Context.IncludeDeclaration); ok {
			return fb, nil
		}
	}
	return s.toLSPLocations(locs), nil
}

// packageNameReferencesFallback answers handleReferences' file-local
// fallback for a package-QUALIFIER identifier (the "os" in "os.Getenv"):
// the workspace facts index never records a *types.PkgName as an indexable
// symbol (it names an import, not a workspace declaration), so
// resolver.References either errors ("no symbol at this position") or,
// once the index does answer for some other reason, still cannot have
// found anything for this position -- either way this tries
// langfeat.PackageNameReferences against the type-checked file directly,
// mirroring definitionFallback's identical index-independent path on the
// definition side (see langfeat.PackageNameDefinition's doc for the same
// root cause). Scope is deliberately file-local, not workspace-wide: a
// package qualifier is local to the importing file by construction (see
// PackageNameReferences' doc), so this already closes the gap a user
// expects clicking a qualifier without needing the facts index at all. ok
// is false if offset is not on a *types.PkgName identifier, or nothing
// resolved.
func (s *Server) packageNameReferencesFallback(ctx context.Context, u uri.URI, pos protocol.Position, includeDecl bool) (protocol.LocationSlice, bool) {
	cf := s.checkedFile(ctx, u, pos)
	if !cf.ok {
		return nil, false
	}
	ranges, err := langfeat.PackageNameReferences(cf.cp, cf.path, cf.offset, includeDecl)
	if err != nil {
		s.logger.Printf("server: package name references %s: %v", cf.path, err)
		return nil, false
	}
	if len(ranges) == 0 {
		return nil, false
	}
	out := make(protocol.LocationSlice, 0, len(ranges))
	for _, rg := range ranges {
		lspRange, ok := offsetRangeToLSP(cf.text, rg.StartOffset, rg.EndOffset)
		if !ok {
			continue
		}
		out = append(out, protocol.Location{URI: uri.File(cf.path), Range: lspRange})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func (s *Server) handleImplementation(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.ImplementationParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return nil, s.indexUnavailableError("implementation")
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return protocol.LocationSlice(nil), nil
	}
	phaseTimerFrom(ctx).enter("facts.Implementation")
	locs, err := resolver.Implementation(ctx, path, line, col)
	if err != nil {
		// See handleDefinition's comment: most errors here are an ordinary
		// "no symbol at this position" miss, but log it anyway so a
		// genuine facts-read failure does not vanish silently. One
		// specific, real failure mode is resolver.Implementation needing
		// to decode the QUERIED interface/method's OWN declaring
		// package's export data (never a candidate's, which
		// implementingTypes/implementedInterfaces already tolerate -- see
		// internal/xref's implDiag doc) -- a gap internal/xref's doc.go
		// documents and deliberately defers, since that package never
		// sees the LSP session's own live type-checked info. The file the
		// cursor is in is, by construction, the one the user has open, so
		// implementationLiveFallback answers straight from that live
		// go/types data instead (see its own doc for exactly what it
		// covers and what it still cannot).
		s.logger.Printf("server: implementation at %s:%d:%d: %v", path, line, col, err)
		if fb, ok := s.implementationLiveFallback(ctx, p.TextDocument.URI, p.Position); ok {
			return fb, nil
		}
		return protocol.LocationSlice(nil), nil
	}
	return s.toLSPLocations(locs), nil
}

// implementationLiveFallback answers handleImplementation when
// resolver.Implementation could not decode the QUERIED interface or
// method's own declaring package's export data. It resolves the same
// symbol directly from the currently open file's already live, type-
// checked go/types info (no export-data decode needed for the QUERIED
// side at all), then finds workspace-wide implementer candidates from
// EXISTING index data alone -- store.DB.LookupMethod plus
// index.MethodFingerprint, neither of which needs export data when a
// candidate's recorded fingerprint matches (see internal/index's
// registerMethodSet doc) -- without importing or modifying internal/xref
// or internal/index. Only the interface -> implementer(s) direction is
// covered (an interface method or the interface's own name): the reverse,
// concrete type -> interfaces direction is decode-based even in
// internal/xref itself (see implementedInterfaces' own doc for why), so a
// fingerprint-only fallback would gain nothing there. A candidate this
// pass cannot confirm from the index alone (a generic receiver, or a
// genuine fingerprint mismatch a decode could still resolve) is simply
// omitted, never guessed at; ok is false whenever there is nothing to
// add, letting the caller fall through to its existing empty result.
func (s *Server) implementationLiveFallback(ctx context.Context, u uri.URI, pos protocol.Position) (protocol.LocationSlice, bool) {
	ws := s.workspace()
	idx := s.idx.Load()
	if ws == nil || idx == nil {
		return nil, false
	}
	cf := s.checkedFile(ctx, u, pos)
	if !cf.ok {
		return nil, false
	}

	if fn, ok := langfeat.CallHierarchyFuncAt(cf.cp, cf.path, cf.offset); ok {
		return s.implementationLiveMethod(ctx, ws, idx, fn)
	}
	if tn, ok := typeNameAt(cf.cp, cf.path, cf.offset); ok {
		if named, ok := tn.Type().(*types.Named); ok && types.IsInterface(named) {
			return s.implementationLiveInterface(ctx, ws, idx, named)
		}
	}
	return nil, false
}

// typeNameAt resolves the identifier at offset (a byte offset from the
// start of file) to the *types.TypeName it denotes, the same identifier
// resolution langfeat.CallHierarchyFuncAt uses for a *types.Func (see its
// doc) applied to a type name instead: cp's own astFileByName/
// posForOffset pair (internal/langfeat/position.go) is unexported, so
// this reimplements that same small, well-established pattern rather
// than exporting it purely for this one caller.
func typeNameAt(cp *check.CheckedPackage, file string, offset int) (*types.TypeName, bool) {
	var astFile *ast.File
	for _, f := range cp.Files() {
		if tf := cp.FileSet().File(f.Pos()); tf != nil && tf.Name() == file {
			astFile = f
			break
		}
	}
	if astFile == nil {
		return nil, false
	}
	tf := cp.FileSet().File(astFile.Pos())
	if offset < 0 || offset > tf.Size() {
		return nil, false
	}
	path, _ := astutil.PathEnclosingInterval(astFile, tf.Pos(offset), tf.Pos(offset))
	for _, n := range path {
		id, ok := n.(*ast.Ident)
		if !ok {
			continue
		}
		tn, ok := cp.Info().ObjectOf(id).(*types.TypeName)
		if !ok || tn.Pkg() == nil {
			return nil, false
		}
		return tn, true
	}
	return nil, false
}

// implementationLiveMethod is implementationLiveFallback's counterpart to
// internal/xref's implementationOfMethod, sourced from fn's live
// *types.Signature instead of a decoded one: fn's receiver -- present for
// an interface method too, since go/types synthesizes one whose type is
// the interface itself, exactly as internal/xref's own methodReceiver
// relies on -- picks the interface -> implementers direction this
// fallback answers; anything else (a concrete method, or a receiver-less
// func) is left to the caller's existing empty result.
func (s *Server) implementationLiveMethod(ctx context.Context, ws *workspace, idx *indexState, fn *types.Func) (protocol.LocationSlice, bool) {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil, false
	}
	recvType := sig.Recv().Type()
	if ptr, ok := recvType.(*types.Pointer); ok {
		recvType = ptr.Elem()
	}
	named, ok := recvType.(*types.Named)
	if !ok || !types.IsInterface(named) {
		return nil, false
	}
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, false
	}
	names, fps, ok := liveInterfaceFingerprints(named, iface)
	if !ok {
		return nil, false
	}
	candidates, ok := s.liveMethodCandidates(ctx, idx, names, fps)
	if !ok {
		return nil, false
	}
	seen := make(map[protocol.Location]bool)
	var out protocol.LocationSlice
	for _, byName := range candidates {
		entries := byName[fn.Name()]
		if len(entries) == 0 {
			continue
		}
		loc, ok := s.liveSymbolLocation(ws, idx, entries[0].MethodPkgHash, entries[0].MethodIDHash)
		if !ok || seen[loc] {
			continue
		}
		seen[loc] = true
		out = append(out, loc)
	}
	return out, len(out) > 0
}

// implementationLiveInterface is implementationLiveFallback's counterpart
// to internal/xref's implementationsOfInterface, sourced from named's
// live *types.Interface instead of a decoded one. Unlike
// implementationsOfInterface it does not also look for embedding
// interfaces: that direction's own candidate confirmation
// (embeddingInterfaces) is decode-based in internal/xref for a reason
// unrelated to the queried side's own decode (see its doc), so this
// narrower, index-only fallback gains nothing by attempting it.
func (s *Server) implementationLiveInterface(ctx context.Context, ws *workspace, idx *indexState, named *types.Named) (protocol.LocationSlice, bool) {
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, false
	}
	names, fps, ok := liveInterfaceFingerprints(named, iface)
	if !ok {
		return nil, false
	}
	candidates, ok := s.liveMethodCandidates(ctx, idx, names, fps)
	if !ok {
		return nil, false
	}
	var out protocol.LocationSlice
	for key := range candidates {
		loc, ok := s.liveSymbolLocation(ws, idx, key.PkgHash, key.TypeSymbolIDHash)
		if !ok {
			continue
		}
		out = append(out, loc)
	}
	return out, len(out) > 0
}

// liveInterfaceFingerprints computes named/iface's own method names and
// canonical signature fingerprints directly from live go/types data,
// using the exported index.MethodFingerprint -- the exact function
// internal/index's registerMethodSet used at index time to record what a
// genuine implementer's own entries carry, so the two compare equal
// without decoding anything. ok is false for the empty interface
// (interface{}/any -- every type trivially implements it; the same
// exclusion internal/xref's implementationsOfInterface applies) or a
// generic interface (registerMethodSet never fingerprints a generic
// receiver, so there is nothing for a fingerprint-only pass to compare
// against).
func liveInterfaceFingerprints(named *types.Named, iface *types.Interface) (names []string, fps map[string]uint64, ok bool) {
	if iface.NumMethods() == 0 || named.TypeParams().Len() > 0 {
		return nil, nil, false
	}
	names = make([]string, iface.NumMethods())
	fps = make(map[string]uint64, iface.NumMethods())
	for i := 0; i < iface.NumMethods(); i++ {
		fn := iface.Method(i)
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			return nil, nil, false
		}
		names[i] = fn.Name()
		fps[fn.Name()] = index.MethodFingerprint(sig)
	}
	return names, fps, true
}

// liveCandKey identifies one indexed candidate type, mirroring internal/
// xref's own candidateKey (unexported there) -- reimplemented here since
// this fallback intentionally never imports internal/xref (see
// implementationLiveFallback's doc).
type liveCandKey struct {
	PkgHash          uint64
	TypeSymbolIDHash uint64
}

// liveMethodCandidates returns, for every candidate concrete type
// recorded under ALL of names in idx's method index (an intersection: a
// real implementer must have every one of them), its store.MethodEntry
// occurrences per name -- mirroring internal/xref's
// candidatesByAllMethods plus its own methodEntriesOfKind Kind-filtering
// (both unexported there), built entirely from store.DB's exported
// LookupMethod and facts blobs. Each candidate is additionally confirmed
// against fps right here (internal/xref's own confirmation is a separate
// pass after gathering; this fallback has no decode fallback to defer
// to, so an unconfirmed candidate is dropped immediately instead of kept
// as an unconfirmed survivor). ok is false only when ctx is canceled
// partway through; a genuinely empty result is a non-error empty map.
func (s *Server) liveMethodCandidates(ctx context.Context, idx *indexState, names []string, fps map[string]uint64) (map[liveCandKey]map[string][]store.MethodEntry, bool) {
	result := make(map[liveCandKey]map[string][]store.MethodEntry)
	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, false
		}
		entries, err := idx.db.LookupMethod(ctx, name)
		if err != nil {
			return nil, false
		}
		set := make(map[liveCandKey][]store.MethodEntry)
		want := fps[name]
		for _, e := range entries {
			if e.Fingerprint == 0 || e.Fingerprint != want {
				continue
			}
			if !s.liveSymbolKindIs(idx, e.PkgHash, e.TypeSymbolIDHash, index.KindType) {
				continue
			}
			if !s.liveSymbolKindIs(idx, e.MethodPkgHash, e.MethodIDHash, index.KindMethod) {
				continue
			}
			key := liveCandKey{PkgHash: e.PkgHash, TypeSymbolIDHash: e.TypeSymbolIDHash}
			set[key] = append(set[key], e)
		}
		if i == 0 {
			for k, es := range set {
				result[k] = map[string][]store.MethodEntry{name: es}
			}
			continue
		}
		for k := range result {
			es, ok := set[k]
			if !ok {
				delete(result, k)
				continue
			}
			result[k][name] = es
		}
		if len(result) == 0 {
			return result, true
		}
	}
	return result, true
}

// liveFactsView returns pkgHash's facts blob as a store.View, mirroring
// internal/xref's own unitBlob (unexported there) but reading directly
// through store.DB/store.CAS's exported API (see implementationLiveMethod's
// doc for why this fallback avoids internal/xref entirely). ok is false
// for any read failure (package not indexed, blob missing, or a decode
// error), treated as an ordinary "this candidate cannot be confirmed"
// miss rather than aborting the whole query, the same tolerance
// internal/xref's own symbolByHash callers apply.
func (s *Server) liveFactsView(idx *indexState, pkgHash uint64) (*store.View, bool) {
	ptr, err := idx.db.GetUnit(context.Background(), pkgHash)
	if err != nil {
		return nil, false
	}
	blob, ok, err := idx.cas.Get(context.Background(), ptr.BlobKey)
	if err != nil || !ok {
		return nil, false
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		return nil, false
	}
	v, err := store.NewView(u.Facts)
	if err != nil {
		return nil, false
	}
	return v, true
}

// liveSymbolKindIs reports whether idHash's symbol in pkgHash's facts is
// recorded with kind want, the same Kind check internal/xref's own
// methodEntriesOfKind applies to filter a stale append-only posting (see
// its doc) before this fallback's fingerprint-only confirmation trusts
// it.
func (s *Server) liveSymbolKindIs(idx *indexState, pkgHash, idHash uint64, want uint8) bool {
	v, ok := s.liveFactsView(idx, pkgHash)
	if !ok {
		return false
	}
	sym, found := v.LookupSymbol(idHash)
	return found && sym.Kind() == want
}

// liveSymbolLocation resolves idHash's declaration in pkgHash's facts to
// an LSP Location, joining a relative stored path onto ws.root exactly as
// internal/xref's own absPath does for RelativeIndexPaths, then applying
// the same dirty-buffer correction toLSPLocations/correctResultLocation
// already give every other cross-reference result.
func (s *Server) liveSymbolLocation(ws *workspace, idx *indexState, pkgHash, idHash uint64) (protocol.Location, bool) {
	v, ok := s.liveFactsView(idx, pkgHash)
	if !ok {
		return protocol.Location{}, false
	}
	sym, found := v.LookupSymbol(idHash)
	if !found {
		return protocol.Location{}, false
	}
	storedPath, err := v.FileAt(int(sym.FileIdx()))
	if err != nil {
		return protocol.Location{}, false
	}
	file := storedPath
	if RelativeIndexPaths(ws.root) && !filepath.IsAbs(storedPath) {
		file = filepath.Join(ws.root, storedPath)
	}
	nameLen := int64(len(sym.Name()))
	if nameLen > math.MaxUint32 {
		return protocol.Location{}, false
	}
	endCol := sym.Col() + uint32(nameLen)
	if endCol < sym.Col() {
		return protocol.Location{}, false
	}
	loc := xref.Location{File: file, Line: sym.Line(), Col: sym.Col(), EndCol: endCol}
	return s.correctResultLocation(loc)
}

func (s *Server) handleWorkspaceSymbol(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.WorkspaceSymbolParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return nil, s.indexUnavailableError("workspace symbol")
	}
	infos, err := resolver.WorkspaceSymbol(ctx, p.Query)
	if err != nil {
		// Unlike Definition/References/Implementation, a WorkspaceSymbol
		// error is never an ordinary "nothing at this position" miss (it
		// takes no position at all) — it always means a genuine DB lookup
		// failure, so it is always worth logging.
		s.logger.Printf("server: workspace symbol %q: %v", p.Query, err)
		return protocol.SymbolInformationSlice(nil), nil
	}
	out := make(protocol.SymbolInformationSlice, 0, len(infos))
	for _, info := range infos {
		loc, ok := s.correctResultLocation(info.Location)
		if !ok {
			continue
		}
		out = append(out, protocol.SymbolInformation{
			BaseSymbolInformation: protocol.BaseSymbolInformation{
				Name:          info.Name,
				Kind:          workspaceSymbolKind(info.Kind),
				ContainerName: &info.Container,
			},
			Location: loc,
		})
	}
	return out, nil
}

func (s *Server) handleRename(ctx context.Context, params json.RawMessage) (any, error) {
	var p protocol.RenameParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	resolver, ok := s.resolverOrWarn()
	if !ok {
		return nil, s.indexUnavailableError("rename")
	}
	path := p.TextDocument.URI.FsPath()
	line, col, ok := s.xrefPosition(path, p.Position)
	if !ok {
		return nil, rpc.NewError(int32(protocol.ErrorCodesInvalidRequest), "golance: no renameable symbol at this position")
	}
	edits, err := resolver.Rename(ctx, path, line, col, p.NewName)
	if err != nil {
		// Most errors here mean "no symbol at this position" or a facts-read
		// gap — a routine outcome its sibling handlers (handleDefinition et
		// al.) already treat as an empty result, not a protocol error. Log
		// it so a genuine fault is still visible, rather than surfacing the
		// raw internal error text to the client.
		s.logger.Printf("server: rename at %s:%d:%d: %v", path, line, col, err)
		return nil, nil
	}

	if dirty := s.dirtyRenameFiles(edits); len(dirty) > 0 {
		// correctResultRange's dirty-buffer correction (see dirty.go) only
		// shifts line numbers via a naive top-down line diff and is blind to
		// column-level edits on the same line, so it can silently drop or
		// misplace occurrences. That is an acceptable simplification for
		// its other, read-only callers (definition/references/workspace
		// symbol: worst case a stale result the user re-navigates from),
		// but not here, where it would silently corrupt a write. Rather than
		// risk a partially-wrong WorkspaceEdit, refuse the whole rename
		// loudly whenever any file it touches has unsaved edits.
		msg := "golance: cannot safely rename while " + strings.Join(dirty, ", ") + " has unsaved edits; save and retry"
		s.logger.Printf("server: rename %q: refusing, unsaved edits could shift occurrence positions in %v", p.NewName, dirty)
		return nil, rpc.NewError(int32(protocol.ErrorCodesInternalError), msg)
	}

	changes := make(map[uri.URI][]protocol.TextEdit, len(edits))
	var unresolved int
	for file, fes := range edits {
		var out []protocol.TextEdit
		for _, e := range fes {
			rng, ok := s.correctResultRange(file, e.Line, e.Col, e.EndCol)
			if !ok {
				unresolved++
				continue
			}
			out = append(out, protocol.TextEdit{Range: rng, NewText: e.NewText})
		}
		if len(out) > 0 {
			changes[uri.File(file)] = out
		}
	}
	if unresolved > 0 {
		// A rename must be all-or-nothing: applying only the references whose
		// range happened to resolve would leave the rest of the occurrences
		// under the old name, silently producing code that no longer
		// compiles with no indication why. Refuse the whole edit instead of
		// returning the partial WorkspaceEdit, the same all-or-nothing
		// contract dirtyRenameFiles enforces above for unsaved edits.
		msg := fmt.Sprintf("golance: cannot safely rename %q; %d reference(s) could not be resolved against the current file contents", p.NewName, unresolved)
		s.logger.Printf("server: rename %q: refusing, %d reference range(s) unresolved", p.NewName, unresolved)
		return nil, rpc.NewError(int32(protocol.ErrorCodesInternalError), msg)
	}
	return &protocol.WorkspaceEdit{Changes: changes}, nil
}
