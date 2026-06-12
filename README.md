# gopls-fleet

An LSP proxy that makes gopls usable in large Go monorepos.

gopls type-checks every workspace package in the background after startup.
In a single-module monorepo with ~24k Go files this costs minutes of CPU and
>10GB of RSS per editor session. gopls-fleet sits between your editor and
gopls, starts the workspace fully excluded (`directoryFilters: ["-**"]`), and
widens the scope automatically as you open files — so gopls only pays for the
services you are actually editing.

## Usage

Point your editor's gopls path at the proxy:

```
gopls-fleet [-gopls /path/to/gopls] [-granularity 3] [-debounce 500ms] [-log /tmp/fleet.log]
```

- `-granularity`: number of path segments that form one scope unit.
  `3` maps `go/services/auth/internal/tokens/x.go` to `go/services/auth`.
- The proxy patches `initialize` options and `workspace/configuration`
  responses, so your other gopls settings pass through untouched.

## Measured on a production monorepo (24k Go files, single go.mod, cold cache)

| metric | plain gopls | gopls-fleet (one service open) |
|---|---|---|
| background type-check CPU | 205.8s | 54.3s (-74%) |
| RSS | 13.1GB | 3.3GB (-75%) |
| opening a second service | n/a | +12.8s to full features |

## Current limitations

- Adding a new directory to the scope re-creates the gopls view, which
  re-runs `go list` (~10-15s on a 3.6k-package repo). A persistent
  incremental `GOPACKAGESDRIVER` is planned to remove this.
- `rename` / `find references` only see the current scope. A reverse-import
  index that auto-expands the scope for those requests is planned.
- The scope only grows during a session; closed services are not yet evicted.
