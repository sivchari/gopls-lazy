# Changelog

## [v0.4.1](https://github.com/sivchari/gopls-lazy/compare/v0.4.0...v0.4.1) - 2026-08-07
- fix: honor DriverRequest build flags in the graph driver by @sivchari in https://github.com/sivchari/gopls-lazy/pull/17

## [v0.4.0](https://github.com/sivchari/gopls-lazy/compare/v0.3.0...v0.4.0) - 2026-08-06
- fix: make definition/references reliable with a persistent worker and config-generation barrier by @sivchari in https://github.com/sivchari/gopls-lazy/pull/12
- fix: hold inlayHint and hover until the file's scope unit is applied by @sivchari in https://github.com/sivchari/gopls-lazy/pull/14
- feat: support editing nested worktree modules from a single session by @sivchari in https://github.com/sivchari/gopls-lazy/pull/15
- release v0.4.0 by @sivchari in https://github.com/sivchari/gopls-lazy/pull/16

## [v0.3.0](https://github.com/sivchari/gopls-lazy/compare/v0.2.0...v0.3.0) - 2026-07-03
- fix: make definition/references work in git worktrees and own directoryFilters exclusively by @sivchari in https://github.com/sivchari/gopls-lazy/pull/9
- release v0.3.0 by @sivchari in https://github.com/sivchari/gopls-lazy/pull/11

## [v0.2.0](https://github.com/sivchari/gopls-lazy/compare/v0.1.1...v0.2.0) - 2026-06-23
- perf: skip competing graph rebuilds for fast warm startup by @sivchari in https://github.com/sivchari/gopls-lazy/pull/5
- feat: isolate cross-reference requests in a worker and serve workspace symbols in-proxy by @sivchari in https://github.com/sivchari/gopls-lazy/pull/6
- release v0.2.0 by @sivchari in https://github.com/sivchari/gopls-lazy/pull/8

## [v0.1.1](https://github.com/sivchari/gopls-lazy/compare/v0.1.0...v0.1.1) - 2026-06-18
- chore: add MIT LICENSE file by @sivchari in https://github.com/sivchari/gopls-lazy/pull/3

## [v0.1.0](https://github.com/sivchari/gopls-lazy/commits/v0.1.0) - 2026-06-15
- release v0.1.0 by @sivchari in https://github.com/sivchari/gopls-lazy/pull/2
