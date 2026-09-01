# Changelog

## [v0.1.10](https://github.com/sivchari/golance/compare/v0.1.9...v0.1.10) - 2026-09-01
- docs: add PR #32 to the v0.1.9 changelog entry by @sivchari in https://github.com/sivchari/golance/pull/34
- feat: index in-package test files for cross-reference queries by @sivchari in https://github.com/sivchari/golance/pull/36
- feat: type-check ad-hoc packages for files outside the workspace graph by @sivchari in https://github.com/sivchari/golance/pull/37
- feat: type-check external _test packages by @sivchari in https://github.com/sivchari/golance/pull/38

## [v0.1.9](https://github.com/sivchari/golance/compare/v0.1.8...v0.1.9) - 2026-09-01
- fix: fall back to type-info definitions when the facts index cannot answer by @sivchari in https://github.com/sivchari/golance/pull/30
- feat: fall back to a session-private index when the shared one is locked by @sivchari in https://github.com/sivchari/golance/pull/32

## [v0.1.8](https://github.com/sivchari/golance/compare/v0.1.7...v0.1.8) - 2026-09-01
- fix: refresh inlay hints and semantic tokens when the workspace becomes ready by @sivchari in https://github.com/sivchari/golance/pull/28

## [v0.1.7](https://github.com/sivchari/golance/compare/v0.1.6...v0.1.7) - 2026-09-01
- feat: resolve unsaved and test files to their directory's package by @sivchari in https://github.com/sivchari/golance/pull/26

## [v0.1.6](https://github.com/sivchari/golance/compare/v0.1.5...v0.1.6) - 2026-09-01
- fix: invalidate the xref resolver cache on reindex and stop retrying failed export recovery by @sivchari in https://github.com/sivchari/golance/pull/23
- fix: convert inlay hint positions incrementally by @sivchari in https://github.com/sivchari/golance/pull/25

## [v0.1.5](https://github.com/sivchari/golance/compare/v0.1.4...v0.1.5) - 2026-08-31
- fix: render short package names in type strings and push inlay hint refreshes by @sivchari in https://github.com/sivchari/golance/pull/19
- fix: resolve go to implementation from method names by @sivchari in https://github.com/sivchari/golance/pull/21
- feat: go to definition into the standard library and module dependencies by @sivchari in https://github.com/sivchari/golance/pull/22

## [v0.1.4](https://github.com/sivchari/golance/compare/v0.1.3...v0.1.4) - 2026-08-31
- docs: add PR #10 to the v0.1.3 changelog entry by @sivchari in https://github.com/sivchari/golance/pull/13
- fix: use the type-checked content itself for position math by @sivchari in https://github.com/sivchari/golance/pull/15
- fix: resolve lint findings from the overlay atomicity change by @sivchari in https://github.com/sivchari/golance/pull/16
- fix: make cross-reference queries cancelable and cache recovered export paths by @sivchari in https://github.com/sivchari/golance/pull/17
- fix: detect deleted module files, fatal reindex persist failures, and pin empty-interface implementations by @sivchari in https://github.com/sivchari/golance/pull/18

## [v0.1.3](https://github.com/sivchari/golance/compare/v0.1.2...v0.1.3) - 2026-08-31
- fix: return empty results instead of internal errors for unresolvable files by @sivchari in https://github.com/sivchari/golance/pull/8
- fix: survive handler panics and fail fast on a locked store by @sivchari in https://github.com/sivchari/golance/pull/9
- fix: serialize index revalidation and bind background work to the session by @sivchari in https://github.com/sivchari/golance/pull/10

## [v0.1.2](https://github.com/sivchari/golance/compare/v0.1.1...v0.1.2) - 2026-08-31
- fix: stop canceling request-driven type checks when edits arrive by @sivchari in https://github.com/sivchari/golance/pull/6

## [v0.1.1](https://github.com/sivchari/golance/compare/v0.1.0...v0.1.1) - 2026-08-31
- feat: add missing LSP features and fix silently stale cross-references by @sivchari in https://github.com/sivchari/golance/pull/4

## [v0.1.0](https://github.com/sivchari/golance/commits/v0.1.0) - 2026-08-28
- feat: initial implementation by @sivchari in https://github.com/sivchari/golance/pull/1
- fix: resolve all lint findings and add tagpr release plumbing by @sivchari in https://github.com/sivchari/golance/pull/2
