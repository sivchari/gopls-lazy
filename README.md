# gopls-fleet

A local-only LSP proxy that makes gopls usable in large Go monorepos.

gopls type-checks every workspace package in the background after startup.
In a single-module monorepo with ~24k Go files this costs minutes of CPU and
>10GB of RSS per editor session. gopls-fleet sits between your editor and
gopls and keeps the workspace scoped to what you are actually editing.
No server, no shared cache, no infrastructure.

## How it works

- **Dynamic scoping**: gopls starts with everything excluded
  (`directoryFilters: ["-**"]`) and the scope widens automatically as you
  open files.
- **Two-stage rescope**: an opened file is served immediately in gopls's
  orphan mode (diagnostics in ~1s); the workspace reload happens right
  after, so first feedback is never blocked.
- **Reverse-import index**: `rename` / `references` / `implementation`
  requests are held while the scope expands with the reverse-import closure
  of the target package (built by parsing import clauses only; 24k files
  index in ~1.6s), so results are never silently truncated.
- **GOPACKAGESDRIVER graph cache**: the same binary acts as a
  `GOPACKAGESDRIVER`. The proxy caches the package graph for the workspace
  load pattern and serves every re-scope from memory, eliminating the
  repeated `go list ./...` (10-15s each on a 3.6k-package repo). Unknown or
  stale queries return `NotHandled` and fall back to the real go list, so
  correctness never depends on the cache.

## Usage

Point your editor's gopls path at the proxy:

```
gopls-fleet [-gopls /path/to/gopls] [-granularity 3] [-debounce 500ms] [-driver=true] [-log /tmp/fleet.log]
```

- `-granularity`: number of path segments that form one scope unit.
  `3` maps `go/services/auth/internal/tokens/x.go` to `go/services/auth`.
- `-driver=false`: disable the GOPACKAGESDRIVER cache and always use go list.
- Other gopls settings pass through untouched (the proxy patches
  `initialize` options and `workspace/configuration` responses only).

## Measured on a production monorepo (24k Go files, single go.mod, cold cache)

| metric | plain gopls | gopls-fleet |
|---|---|---|
| background type-check CPU | 205.8s | 101.2s |
| RSS while editing one service | 13.1GB | 3.7GB |
| first diagnostics on open | 0.6s | 1.4s |
| opening a second service, full features | n/a | 4.3s |
| references on a package outside the scope | partial results | correct, 3.8s |

The proxy spends ~13s of background CPU once per session building the graph
cache and ~1.6s building the reverse-import index; both are included above.

## Current limitations

- The scope only grows during a session; closed services are not evicted.
- Method renames on types that flow far from their package may still need a
  wider scope than the reverse-import closure of the defining package
  (interface satisfaction across packages). When in doubt, verify with a
  workspace-wide grep before landing a rename.
- `//go:embed` pattern changes do not invalidate the graph cache until the
  next import-affecting save.
