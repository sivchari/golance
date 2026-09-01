// Package xref answers cross-reference queries (definition, references,
// implementation, workspace/symbol, rename) directly from the on-disk facts
// index (internal/store) plus the import graph (internal/graph), without any
// LSP protocol dependency.
//
// Positions in and out of this package are (file path, line, col) in the
// same coordinate system internal/index writes into facts: 1-based line and
// 1-based byte column, matching go/token.Position ("Column is the column
// number, in bytes, starting at 1"), not UTF-16 code units. Converting
// to/from an editor's own coordinate system is the caller's responsibility.
//
// References and Rename read only the defining package's facts blob plus
// the facts blobs of packages in its reverse-dependency closure
// ([internal/graph.Snapshot.ClosureUnits]), so their cost scales with the
// number of packages that could reference the symbol and the number of
// results, not with workspace size.
//
// Implementation resolves candidates in two passes: a name-based first pass
// over [internal/store.DB.LookupMethod] that is sound (never omits a real
// implementer) but can over-approximate, followed by a confirmation pass.
// For the interface -> implementers direction (implementingTypes), that
// confirmation compares each candidate's index-recorded, canonical
// signature fingerprint (internal/index.MethodFingerprint) against the
// queried interface's own — needing no export data at all, which is what
// makes an UNEXPORTED implementer resolvable (export data only ever
// carries exported package-scope objects) and immune to a second class of
// false negative go/types.Implements is prone to: two independently
// decoded packages referencing what is structurally the same dependency
// type are, to go/types, two DIFFERENT *types.Named objects unless decoded
// through a shared imports map, so types.Implements can report "does not
// implement" for a candidate that genuinely does (see implementingTypes'
// own doc for the full soundness argument). A candidate whose receiver is
// generic (fingerprinting excluded, see internal/index's registerMethodSet)
// or whose fingerprint does not match still falls back to loading its
// export data and confirming with go/types.Implements, exactly as every
// candidate used to before this fix. The concrete type -> interfaces
// direction (implementedInterfaces) keeps the original decode-based
// confirmation unconditionally — see its own doc for why that asymmetry is
// intentional. A [Resolver] decodes export data through one shared
// internal/typecheck.Cache and token.FileSet for its lifetime, so that a
// decode-based confirmation sees consistent object identity for any
// package an interface and a candidate both depend on. Neither is evicted,
// so long-lived Resolvers should be recycled periodically.
//
// An implementation query's own diagnostics (implDiag/logImplDiag, in
// implementation.go) never see the LSP session's live, already
// type-checked CheckedPackage for the queried file -- this package only
// ever reads back its own facts index and previously-written export data,
// never the check.Engine that produced them. So when the QUERIED
// interface's (or type's) own export data itself fails to decode --
// Resolver.Implementation's own resolveNamed/resolveMethodFunc call, not a
// candidate's, in implementingTypes/implementedInterfaces's loop -- there is
// no live types.Info to fall back to here; the query simply returns that
// decode error (already logged with the failing package's path by
// internal/server's handleImplementation) rather than degrading to a
// partial answer. Threading a CheckedPackage through this package's
// otherwise index/export-data-only API is a bigger architectural change
// than this diagnostics pass justifies on its own; left as a follow-up if
// the interface side of implDiag's own decode-failure case turns out to be
// the actual root cause of a future report, rather than a candidate's (the
// case this pass could reproduce and fix -- see implementation_decodefailure_test.go).
package xref
