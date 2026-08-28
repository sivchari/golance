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
// implementer) but can over-approximate, followed by loading each surviving
// candidate's export data and confirming with go/types.Implements. A
// [Resolver] decodes export data through one shared internal/typecheck.Cache
// and token.FileSet for its lifetime, so that types.Implements sees
// consistent object identity for any package an interface and a candidate
// both depend on. Neither is evicted, so long-lived Resolvers should be
// recycled periodically.
package xref
