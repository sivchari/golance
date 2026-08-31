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

## Development

### Linting

CI runs `golangci-lint run ./...` via [`.github/workflows/lint.yaml`](.github/workflows/lint.yaml)
(golangci-lint `latest`, config in [`.golangci.yaml`](.golangci.yaml)). To reproduce
that locally:

```sh
make lint-local
```

If your `go` in `PATH` is a non-release build (e.g. a `go1.27-devel` custom
toolchain), plain `golangci-lint run ./...` can fail with
`context loading failed: no go files to analyze`, because golangci-lint shells
out to `go list -json` to load packages and that call breaks on devel builds.
`make lint-local` points `GOROOT`/`PATH` at a stock SDK for the lint run while
leaving your default `go` untouched. It prefers the toolchain the go command
already downloaded for `go.mod`'s `go` directive (present in the module cache
after any build, no network needed) and falls back to installing one through
`golang.org/dl`. See the `lint-local` target in the [`Makefile`](Makefile) for
details.
