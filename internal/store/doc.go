// Package store implements golance's on-disk facts index as two layers (see
// plan-feat-v0.1.md and research-feat-v0.1.md's "追記 3" for the design
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
//     plus the name/method/SymbolID-string lookup indices built from those
//     blobs. Being per-root, it is never shared or contended across golance
//     sessions the way a single repository-wide database would be.
//
// # Zero-copy facts blobs
//
// A [UnitBlob]'s Facts field is never fully decoded into Go structs. [View]
// wraps the raw []byte and answers queries (SymbolAt, LookupSymbol, RefsAt,
// RefsTo) by computing offsets into the blob directly, so a single
// hover/definition query touches only the bytes it needs, regardless of how
// many symbols the package has. This is what keeps query latency
// independent of package or workspace size (see plan-feat-v0.1.md, "9秒問題").
// A blob returned by [CAS.Get] is an ordinary heap-allocated []byte
// (os.ReadFile's result, not a memory-mapped bbolt transaction view), so —
// unlike DB's own small posting-list buckets — it carries no
// transaction-lifetime restriction: callers may retain it as long as they
// like.
package store
