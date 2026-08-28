// Package langfeat implements golance's interactive LSP features — hover,
// completion, signature help, document symbols, inlay hints, and
// formatting — over a single type-checked package (internal/check). It is
// independent of the LSP wire protocol: positions in and out of this
// package are plain byte offsets from the start of a file, and results are
// plain Go values. Converting to/from go.lsp.dev/protocol types and UTF-16
// positions is the server layer's job.
package langfeat
