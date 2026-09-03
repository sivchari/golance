// Package store implements golance's on-disk facts index as two layers (see
// plan-feat-v0.1.md and research-feat-v0.1.md's "Addendum 3" for the design
// rationale):
//
//   - [CAS]: a shared, lock-free, content-addressed blob store, one per
//     repository (every worktree of a repository points at the same CAS
//     directory — see internal/server's repoKey). Each package's facts blob,
//     export data, per-file stat snapshot, and index-entry contribution for
//     one exact source version are bundled into one immutable [UnitBlob],
//     stored under a key deterministically derived from that content (see
//     internal/index's key computation) — the same principle GOCACHE uses
//     for build artifacts, so two sessions computing the same key race onto
//     the same bytes harmlessly instead of needing a lock.
//   - [DB]: a small, per-root (per-worktree) bbolt database recording only
//     which CAS blob key each package currently resolves to ([UnitPointer]),
//     plus the name/method/SymbolID-string/reference-posting lookup indices
//     built from those blobs. The reference-posting index (bucketRefPostings,
//     queried via [DB.PostingsFor]; see postings.go) is the odd one out
//     among these: unlike the others, which tolerate a stale entry as
//     harmless (see applyIndexEntries's doc), a package's postings are
//     replaced EXACTLY on every reindex, via a per-source manifest
//     (bucketRefPostingManifest; see applyPostings) — internal/xref's
//     References needs it to never lag or outlive the facts it mirrors.
//     Being per-root, DB is never shared or contended across golance
//     sessions the way a single repository-wide database would be.
//
// # Zero-copy facts blobs
//
// A [UnitBlob]'s Facts field is never fully decoded into Go structs. [View]
// wraps the raw []byte and answers queries (SymbolAt, LookupSymbol, RefsAt,
// RefsTo) by computing offsets into the blob directly, so a single
// hover/definition query touches only the bytes it needs, regardless of how
// many symbols the package has. This is what keeps query latency
// independent of package or workspace size (see plan-feat-v0.1.md, "the
// 9-second problem").
// A blob returned by [CAS.Get] is an ordinary heap-allocated []byte
// (os.ReadFile's result, not a memory-mapped bbolt transaction view), so —
// unlike DB's own small posting-list buckets — it carries no
// transaction-lifetime restriction: callers may retain it as long as they
// like.
//
// # CAS garbage collection
//
// A [CAS] directory never deletes anything on its own: every source edit or
// factsSchemaVersion bump (internal/index) computes a new key and Puts a
// new blob, orphaning whatever the old key pointed at. [CAS.GC] reclaims
// those orphans via mark-and-sweep, not age alone: a blob survives if it is
// in the caller-supplied mark set (the union of every UnitPointer.BlobKey
// across every [DB] that shares this CAS directory — see internal/server's
// RunCASGC, which builds that union across worktrees via each database's
// [DB.CASDir] meta value) or was written within [GraceWindow]. The whole
// design leans on one property everywhere else in this package already
// assumes: a missing blob is never a correctness bug, only a cache miss —
// [CAS.Get] reports ok=false exactly as it would for a key nothing ever
// Put, and the caller retypechecks and re-Puts it, precisely like a GOCACHE
// eviction. GC's mark set is therefore allowed to be a best-effort
// under-approximation (see RunCASGC's own doc on why a currently-locked
// database is skipped rather than waited for) without risking anything
// worse than an occasional avoidable recompute.
package store
