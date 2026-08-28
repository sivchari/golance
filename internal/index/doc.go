// Package index builds golance's on-disk facts database from a
// graph.Snapshot: it type-checks every workspace package in dependency
// order, extracts cross-reference facts into internal/store, and evicts
// each dependency's decoded *types.Package as soon as every package that
// imports it has finished, so peak memory stays proportional to the bounded
// worker count rather than to workspace size.
package index
