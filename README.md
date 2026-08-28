# golance

A memory-bounded LSP server for Go, built for large monorepos.

golance keeps its resident memory independent of workspace size. It indexes
the whole workspace once, persists the analysis to disk (content-hash keyed,
so editor restarts never re-index), and answers cross-reference queries by
reading the on-disk index on demand. While you edit, only the package you
are editing is type-checked, with dependencies resolved from export data.

Inspired by [rust-glancer](https://rust-glancer.github.io/blog/hello-world/)'s
frozen-analysis design, reusing Go's own `go/parser` + `go/types` for
correctness. Successor to [gopls-lazy](https://github.com/sivchari/gopls-lazy).

Status: under active development, not yet usable. See `plan-feat-v0.1.md`.
