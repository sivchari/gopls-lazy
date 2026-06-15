package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/packages"
)

// graphServer holds a cached go/packages driver response for the workspace
// load pattern gopls uses, so re-scoping (which re-creates the gopls view)
// stops paying for a full `go list ./...` every time. Anything it cannot
// answer confidently is delegated back to the real go list via NotHandled.
//
// Disk cache: after each successful build the graph is written to
// $XDG_CACHE_HOME/gopls-lazy/graph-<root-hash>.json. On the next startup
// the cached graph is loaded immediately so the first workspace query is
// served from disk (µs) rather than from a fresh `go list ./...` (13+ s).
// A background rebuild starts in parallel to validate / refresh the cache.
type graphServer struct {
	idx       *revIndex
	log       *log.Logger
	sockPath  string
	cacheFile string // path to the on-disk graph file; empty if no root yet
	ln        net.Listener

	mu           sync.Mutex
	resp         []byte // cached marshaled DriverResponse
	patternsKey  string
	patterns     []string
	dir          string
	building     bool
	stale        bool
	rebuildTimer *time.Timer
}

// savedGraph is the on-disk format for the graph cache.
type savedGraph struct {
	Resp        []byte   `json:"resp"`
	PatternsKey string   `json:"patternsKey"`
	Patterns    []string `json:"patterns"`
	Dir         string   `json:"dir"`
}

// graphCacheKey returns a stable identifier for the graph cache. All git
// worktrees that share the same origin repository share the same key (via the
// git common dir), so the on-disk cache is built once and reused across
// worktrees instead of being rebuilt per worktree checkout.
//
// Resolution order:
//  1. git common dir — stable across all worktrees of the same repo
//  2. module path from go.mod — stable across branches (unless the module line changes)
//  3. workspace root path — fallback when neither git nor go.mod is available
func graphCacheKey(root string) string {
	// git rev-parse --git-common-dir returns the path to the shared .git
	// directory regardless of which worktree is currently checked out.
	out, err := runGit(root, "rev-parse", "--git-common-dir")
	if err == nil {
		dir := strings.TrimSpace(string(out))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		return dir
	}
	// Fallback: parse the module path from go.mod.
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.SplitN(string(b), "\n", 20) {
			if mod, ok := strings.CutPrefix(line, "module "); ok {
				if mod = strings.TrimSpace(mod); mod != "" {
					return mod
				}
			}
		}
	}
	// Last resort: use the workspace root path directly.
	return root
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = os.Environ()
	return cmd.Output()
}

// graphCacheFile returns the path for the on-disk cache for a given workspace
// root. Uses XDG_CACHE_HOME / darwin UserCacheDir if set, else ~/.cache.
func graphCacheFile(root string) string {
	key := graphCacheKey(root)
	h := sha256.Sum256([]byte(key))
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(base, "gopls-lazy", fmt.Sprintf("graph-%x.json", h[:8]))
}

type driverQuery struct {
	Patterns []string
	Dir      string
	Request  json.RawMessage
}

// startGraphServer starts the GOPACKAGESDRIVER unix socket server.
// Call setRoot once the workspace root is known (on initialize) so the
// on-disk cache can be located and loaded before the first driver query.
func startGraphServer(idx *revIndex, logger *log.Logger) (*graphServer, error) {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("gopls-lazy-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	g := &graphServer{idx: idx, log: logger, sockPath: sock, ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go g.handle(conn)
		}
	}()
	return g, nil
}

// setRoot wires up the on-disk cache path and starts loading any existing
// cache. It must be called once, before the first GOPACKAGESDRIVER query.
func (g *graphServer) setRoot(root string) {
	g.mu.Lock()
	if g.cacheFile != "" {
		g.mu.Unlock()
		return // already set
	}
	g.cacheFile = graphCacheFile(root)
	g.mu.Unlock()
	g.loadDiskCache()
}

// loadDiskCache reads the on-disk graph and begins a background rebuild to
// validate / refresh it. Callers must NOT hold g.mu.
func (g *graphServer) loadDiskCache() {
	if g.cacheFile == "" {
		return
	}
	data, err := os.ReadFile(g.cacheFile)
	if err != nil {
		return // no cache yet (first run)
	}
	var saved savedGraph
	if err := json.Unmarshal(data, &saved); err != nil {
		g.log.Printf("driver: disk cache corrupt, ignoring: %v", err)
		return
	}
	if len(saved.Resp) == 0 || saved.PatternsKey == "" || saved.Dir == "" {
		return
	}
	g.mu.Lock()
	g.resp = saved.Resp
	g.patternsKey = saved.PatternsKey
	g.patterns = saved.Patterns
	g.dir = saved.Dir
	g.stale = false
	g.building = true // prevent answer() from spawning a duplicate rebuild
	g.mu.Unlock()
	g.log.Printf("driver: loaded disk cache (%d bytes) from %s; refreshing in background", len(saved.Resp), g.cacheFile)
	// Rebuild in background to pick up any changes since the cache was written.
	go g.build(saved.Patterns, saved.Dir, saved.PatternsKey)
}

// saveDiskCache writes the current graph to the on-disk cache file.
// Callers must NOT hold g.mu.
func (g *graphServer) saveDiskCache(resp []byte, patternsKey string, patterns []string, dir string) {
	if g.cacheFile == "" {
		return
	}
	saved := savedGraph{
		Resp:        resp,
		PatternsKey: patternsKey,
		Patterns:    patterns,
		Dir:         dir,
	}
	b, err := json.Marshal(saved)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.cacheFile), 0o755); err != nil {
		return
	}
	tmp := g.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, g.cacheFile); err != nil {
		_ = os.Remove(tmp)
		return
	}
	g.log.Printf("driver: saved disk cache (%d bytes) to %s", len(b), g.cacheFile)
}

func (g *graphServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var q driverQuery
	if err := json.NewDecoder(conn).Decode(&q); err != nil {
		return
	}
	_, _ = conn.Write(g.answer(q))
}

var notHandled = []byte(`{"NotHandled":true}`)

func (g *graphServer) answer(q driverQuery) []byte {
	var req packages.DriverRequest
	if err := json.Unmarshal(q.Request, &req); err != nil {
		return notHandled
	}
	key := strings.Join(q.Patterns, "\x00")

	g.mu.Lock()
	resp := g.resp
	stale := g.stale
	hasCache := resp != nil && key == g.patternsKey

	if !hasCache {
		// No cache at all: trigger a background build for workspace queries
		// and tell gopls to fall back to the real go list.
		if isWorkspaceQuery(q.Patterns) && !g.building {
			g.building = true
			patterns := append([]string(nil), q.Patterns...)
			dir := q.Dir
			go g.build(patterns, dir, key)
		}
		g.mu.Unlock()
		g.log.Printf("driver: NotHandled (no cache, patterns=%v)", q.Patterns)
		return notHandled
	}

	// We have a cache. If it is stale (go.mod / imports changed on disk),
	// kick off a background rebuild but still serve the cached data so
	// re-scopes during the ~13s rebuild window don't regress to full go list.
	if stale && !g.building {
		g.building = true
		patterns, dir := g.patterns, g.dir
		go g.build(patterns, dir, key)
	}
	g.mu.Unlock()

	// Only fall back for live import changes the user has in an unsaved
	// overlay — those modify the package graph in a way the cached snapshot
	// cannot reflect.
	if g.overlayDirty(req.Overlay) {
		g.log.Printf("driver: overlay changes imports, falling back to go list")
		return notHandled
	}
	if stale {
		g.log.Printf("driver: served %d patterns from stale cache (%d bytes, rebuild in progress)", len(q.Patterns), len(resp))
	} else {
		g.log.Printf("driver: served %d patterns from cache (%d bytes)", len(q.Patterns), len(resp))
	}
	return resp
}

// isWorkspaceQuery reports whether the patterns look like gopls's initial
// workspace load (recursive patterns), as opposed to file= or single-package
// queries which the real go list answers quickly anyway.
func isWorkspaceQuery(patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/...") {
			return true
		}
	}
	return false
}

func (g *graphServer) build(patterns []string, dir, key string) {
	start := time.Now()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule |
			packages.NeedTypesSizes | packages.NeedEmbedFiles | packages.NeedEmbedPatterns |
			packages.NeedForTest,
		Dir:   dir,
		Tests: true,
		Env:   append(os.Environ(), "GOPACKAGESDRIVER=off"),
	}
	roots, err := packages.Load(cfg, patterns...)
	if err != nil {
		g.log.Printf("driver: build failed: %v", err)
		g.mu.Lock()
		g.building = false
		g.mu.Unlock()
		return
	}
	var all []*packages.Package
	packages.Visit(roots, func(p *packages.Package) bool {
		all = append(all, p)
		return true
	}, nil)
	rootIDs := make([]string, 0, len(roots))
	for _, p := range roots {
		rootIDs = append(rootIDs, p.ID)
	}
	resp := packages.DriverResponse{
		Compiler:  "gc",
		Arch:      runtime.GOARCH,
		GoVersion: goMinor(runtime.Version()),
		Roots:     rootIDs,
		Packages:  all,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		g.log.Printf("driver: marshal failed: %v", err)
		g.mu.Lock()
		g.building = false
		g.mu.Unlock()
		return
	}
	g.mu.Lock()
	g.resp = b
	g.patterns = patterns
	g.patternsKey = key
	g.dir = dir
	g.stale = false
	g.building = false
	g.mu.Unlock()
	g.log.Printf("driver: graph built in %s (%d packages, %d roots, %dMB)",
		time.Since(start).Round(time.Millisecond), len(all), len(rootIDs), len(b)>>20)
	go g.saveDiskCache(b, key, patterns, dir)
}

// overlayDirty reports whether any open-file overlay changes a file's import
// set compared to the on-disk state the cache was built from.
func (g *graphServer) overlayDirty(overlay map[string][]byte) bool {
	for path, content := range overlay {
		if !strings.HasSuffix(path, ".go") {
			return true
		}
		if !g.idx.SameImports(path, content) {
			return true
		}
	}
	return false
}

// MarkStale schedules a background rebuild; until it finishes, queries fall
// back to the real go list.
func (g *graphServer) MarkStale(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.patternsKey == "" {
		return // never built; nothing to refresh
	}
	if !g.stale {
		g.log.Printf("driver: cache marked stale (%s)", reason)
	}
	g.stale = true
	if g.rebuildTimer != nil {
		g.rebuildTimer.Stop()
	}
	g.rebuildTimer = time.AfterFunc(3*time.Second, func() {
		g.mu.Lock()
		if g.building {
			g.mu.Unlock()
			return
		}
		g.building = true
		patterns, dir, key := g.patterns, g.dir, g.patternsKey
		g.mu.Unlock()
		g.build(patterns, dir, key)
	})
}

var goVersionRe = regexp.MustCompile(`go1\.(\d+)`)

func goMinor(version string) int {
	m := goVersionRe.FindStringSubmatch(version)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
