# gopls-lazy

A local-only LSP proxy that makes gopls usable in large Go monorepos.

gopls type-checks every workspace package in the background after startup.
In a single-module monorepo with ~24k Go files this costs minutes of CPU and
>10GB of RSS per editor session. gopls-lazy sits between your editor and
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
- **Fast warm start**: the on-disk graph is served immediately, and the
  revalidating `go list ./...` is deferred past the initial burst of file
  opens (sooner when a module file changed, later when nothing did) so it
  never competes with type-checking during startup. The cache is invalidated
  only by module-file changes and by changes to files an `//go:embed` pattern
  actually covers — not by every unrelated non-Go file, which would otherwise
  re-list the whole module on editor file-watch noise.

## Usage

```
gopls-lazy [flags]

  -gopls path        gopls binary (default: "gopls" from PATH)
  -granularity n     path segments per scope unit (default 3:
                     go/services/auth/internal/x.go -> go/services/auth)
  -debounce dur      coalesce window for scope changes (default 500ms)
  -evict dur         drop units with no open files after this idle time
                     (default 10m; 0 disables)
  -driver=bool       GOPACKAGESDRIVER graph cache (default true)
  -log path          debug log
```

Unrecognized flags are forwarded to gopls, so the proxy is a drop-in
replacement for the gopls binary. Every flag can also be set via environment
variables (`GOPLS_LAZY_GOPLS`, `GOPLS_LAZY_GRANULARITY`,
`GOPLS_LAZY_DEBOUNCE`, `GOPLS_LAZY_EVICT`, `GOPLS_LAZY_DRIVER=false`,
`GOPLS_LAZY_LOG`) for editors that cannot pass arguments.

### VS Code

```jsonc
// settings.json
{
  "go.alternateTools": { "gopls": "/path/to/gopls-lazy" }
}
```

### Neovim (nvim-lspconfig)

```lua
require("lspconfig").gopls.setup({
  cmd = { "gopls-lazy" },
})
```

Other gopls settings pass through untouched (the proxy patches
`initialize` options and `workspace/configuration` responses only).

## Measured on a production monorepo (24k Go files, single go.mod, cold cache)

| metric | plain gopls | gopls-lazy |
|---|---|---|
| background type-check CPU | 205.8s | 101.2s |
| RSS while editing one service | 13.1GB | 3.7GB |
| first diagnostics on open | 0.6s | 1.4s |
| opening a second service, full features | n/a | 4.3s |
| references on a package outside the scope | partial results | correct, 3.8s |

The proxy spends ~1.6s building the reverse-import index per session. The
~13s graph-cache build is served from disk on warm starts and revalidated in
the background, deferred so it does not compete with the first file opens.

## Scope lifecycle

- Opening a file adds its scope unit; closing all files in a unit evicts it
  after `-evict` (default 10m) of inactivity.
- rename/references resolve the symbol's defining package first (a
  definition request to gopls), then expand to its reverse-import closure.
- Method symbols and `implementation` requests temporarily widen to the
  whole workspace — methods can be reached through interfaces from packages
  that never import the defining package, so the closure is not enough. The
  widening expires after the eviction TTL.
- `//go:embed` directive changes and changes to files an embed pattern
  actually covers invalidate the graph cache (it rebuilds in the background;
  queries fall back to go list until it is fresh). Unrelated non-Go file
  events are ignored, so file-watch noise does not trigger a full re-list.

## Current limitations

- References at a call site resolve through `textDocument/definition`; if
  the definition itself is not resolvable in the current scope (rare: the
  requesting file's package always has its dependencies loaded), the proxy
  falls back to the requesting file's package for closure computation.
- A whole-workspace widening on a big monorepo costs what plain gopls costs
  on every startup; with a warm gopls cache it is seconds, cold it can be
  minutes of background CPU.
