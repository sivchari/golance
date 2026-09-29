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
// References and Rename resolve their target's own declaration from its
// defining package's facts blob, then answer via a single
// [internal/store.DB.PostingsFor] prefix scan over the persisted reverse
// reference index (internal/store's bucketRefPostings, populated during
// facts extraction alongside the facts blob itself — see
// internal/index/facts.go's addRef): cost scales with the number of
// packages that actually reference the symbol and the number of results,
// not with the number of packages that transitively COULD (the
// reverse-dependency closure a naive implementation would otherwise have to
// walk and read in full).
//
// Implementation, References, and TypeHierarchy's Supertypes/Subtypes all
// resolve candidates in two passes: a name-based first pass over
// [internal/store.DB.LookupMethod] that is sound (never omits a real
// implementer) but can over-approximate, followed by a confirmation pass.
// Confirmation compares each candidate's index-recorded, canonical
// signature fingerprint (internal/index.MethodFingerprint) against the
// OTHER side's own method set — needing no export data at all on the side
// being confirmed, which is what makes an UNEXPORTED or _test.go-declared
// type or interface resolvable on EITHER side of an implementation
// relationship (export data only ever carries exported, non-test
// package-scope objects), and immune to a second class of false negative
// go/types.Implements is prone to: two independently decoded packages
// referencing what is structurally the same dependency type are, to
// go/types, two DIFFERENT *types.Named objects unless decoded through a
// shared imports map, so types.Implements can report "does not implement"
// for a candidate that genuinely does (see implementingTypesConfirm's own
// doc for the full soundness argument).
//
// Both a query's own TARGET (methodReceiverKey, ownMethodEntries) and every
// CANDIDATE it is confirmed against (implementingTypesConfirm/
// implementingTypesConfirmByFacts, implementedInterfacesConfirm/
// implementedInterfacesConfirmByKey, embeddingInterfacesConfirm/
// embeddingInterfacesConfirmByFacts, confirmSupertypeCandidate,
// confirmSubtypeCandidate, receiverSatisfiesCandidateInterface) have their
// own facts-only path for exactly this reason: a target or candidate whose
// own export data cannot decode no longer drops out of a query's result
// silently, or fails the query outright, the way it did before this
// package's own fingerprint-based confirmation existed.
//
// Decoding an export data blob (via resolveNamed/resolveMethodFunc) remains
// the fallback in exactly two cases, both left unaddressed deliberately
// rather than built out further:
//
//   - A GENERIC target or candidate (registerMethodSet/
//     registerInterfaceMethodSet's type-parameter exclusion, see their own
//     doc): a still-generic method signature is not canonically comparable
//     by fingerprint, so confirmation falls back to a live decode plus
//     go/types.Implements for that one type/interface, exactly as every
//     type used to before this package's fingerprint-based confirmation
//     existed. This is the ordinary, already-working generics case
//     (implementation_unexported_test.go's
//     TestImplementation_Generic*FallsBackToDecode pin it), NOT a gap.
//   - A type or interface that is BOTH generic AND itself unexported or
//     _test.go-declared: fingerprinting cannot confirm it (generic), and
//     decoding it cannot either (export data never carries it). This
//     doubly-rare combination — unlike either condition alone — has no
//     sound confirmation path available at all without a deeper change to
//     how facts extraction records generic receivers, and is left as a
//     documented, open gap rather than addressed here; see
//     implementingTypesConfirmByFacts, receiverSatisfiesMethodEntries, and
//     confirmSubtypeCandidate's own docs for exactly where it surfaces.
//
// A [Resolver] decodes export data through one shared internal/typecheck.
// Cache and token.FileSet pair, reused across every query for the
// Resolver's whole lifetime (indexState installs a fresh Resolver on every
// reindex, so a stale pair never outlives the index generation it was
// decoded against), so a decode-based confirmation sees consistent object
// identity for any package an interface and a candidate both depend on, and
// a repeat query re-decodes nothing. That pair is discarded and replaced
// wholesale, never piecemeal, once it grows past a byte budget (see
// Resolver.exportCache), the same bound r.units already applies to the raw
// facts/export blob cache.
//
// An implementation query's own diagnostics (implDiag/logImplDiag, in
// implementation.go) never see the LSP session's live, already
// type-checked CheckedPackage for the queried file -- this package only
// ever reads back its own facts index and previously-written export data,
// never the check.Engine that produced them. So when a genuinely-generic
// target's or candidate's own export data itself fails to decode in the
// fallback path above, there is no live types.Info to fall back to further;
// the query either returns that decode error (a target, since nothing else
// could answer it) or silently drops that one candidate (skipCandidate/skip,
// already logged via logImplDiag on an empty result) rather than degrading
// further. Threading a CheckedPackage through this package's otherwise
// index/export-data-only API is a bigger architectural change than this
// diagnostics pass justifies on its own; left as a follow-up if the
// doubly-generic-and-undecodable case above turns out to be the actual root
// cause of a future report.
package xref
