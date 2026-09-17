// Package typedep testdata for internal/typecheck: exports a named type
// that typeuser embeds in its own exported API, so a deep gcexportdata
// blob for typeuser actually references typedep in its manifest (a plain
// function-body call does not — the non-shallow writer only records a
// package for an object it writes, and function bodies are never written).
package typedep

// Greeting is embedded in typeuser's exported API.
type Greeting string
