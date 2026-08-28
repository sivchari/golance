package store

import "hash/fnv"

// Hash returns a deterministic 64-bit hash of s (FNV-1a). It is stable
// across process restarts and Go versions, which matters because the
// result is persisted to disk as facts-blob record fields and bbolt keys.
//
// A 64-bit hash cannot be collision-free for an unbounded number of inputs.
// Callers that need to detect a collision (e.g. resolving a SymbolID hash
// back to its source string) should also persist the original string via
// [DB.PutSymbolIDString] and verify with [DB.VerifySymbolIDString].
func Hash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// BuildSymbolID returns the canonical SymbolID string for a symbol: its
// package's import path joined with its objectpath-encoded path. Generating
// objPath (via golang.org/x/tools/go/types/objectpath) is the index
// layer's responsibility; store only combines and hashes the result.
func BuildSymbolID(pkgPath, objPath string) string {
	return pkgPath + "#" + objPath
}
