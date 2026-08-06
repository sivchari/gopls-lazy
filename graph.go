package goplslazy

import (
	"bytes"
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

	"golang.org/x/mod/modfile"
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
// The revalidating rebuild always runs but is deferred past the initial burst
// of file opens (sooner when a module file changed, later when nothing did),
// so it never competes with type-checking during startup.
type graphServer struct {
	idx       *revIndex
	log       *log.Logger
	sockPath  string
	cacheFile string // path to the on-disk graph file; empty if no root yet
	ln        net.Listener

	// modRoots detects which nested module (e.g. a git worktree with its own
	// go.mod) a query's directory belongs to, so it can be routed to that
	// module's own subslot instead of the main one. indexFor returns the
	// reverse-import index owning a module root, used by a subslot's
	// overlayDirty check. Both nil (a graphServer built directly, e.g. by an
	// existing test) means "no nested modules": every query is answered from
	// the main slot, exactly as before subslots existed.
	modRoots *moduleRootCache
	indexFor func(string) *revIndex

	mu       sync.Mutex
	root     string // workspace root, for the startup freshness check
	main     graphSlot
	subslots map[string]*graphSlot // nested module root -> its own in-memory-only slot, built lazily via subslotFor
}

// graphSlot holds the mutable, per-workspace go/packages driver cache: the
// cached DriverResponse, the pattern set it was built for, its build
// directory, in-flight/staleness bookkeeping, and the //go:embed footprint
// used to decide whether a non-Go file change can affect the graph.
// Protected by the owning graphServer's mu.
type graphSlot struct {
	resp         []byte // cached marshaled DriverResponse
	patternsKey  string
	patterns     []string
	dir          string
	building     bool
	stale        bool
	rebuildTimer *time.Timer

	// //go:embed footprint, so a non-Go file change invalidates the graph only
	// when it can actually affect it (rather than on every build artifact).
	embedReady    bool
	embedFiles    map[string]bool // absolute paths of currently embedded files
	embedPrefixes []string        // slash literal roots of embed patterns (new files)
}

// savedGraph is the on-disk format for the graph cache.
type savedGraph struct {
	Resp        []byte   `json:"resp"`
	PatternsKey string   `json:"patternsKey"`
	Patterns    []string `json:"patterns"`
	Dir         string   `json:"dir"`
	Root        string   `json:"root"` // workspace root the graph was built for
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
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil { //nolint:gosec // reading go.mod from workspace root is intentional
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
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // dir is the workspace root provided by the editor
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
// modRoots and indexFor wire up per-module subslot dispatch (see
// graphServer.answer); either may be nil, meaning no nested modules.
func startGraphServer(idx *revIndex, modRoots *moduleRootCache, indexFor func(string) *revIndex, logger *log.Logger) (*graphServer, error) {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("gopls-lazy-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	g := &graphServer{idx: idx, modRoots: modRoots, indexFor: indexFor, log: logger, sockPath: sock, ln: ln}
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
	g.root = root
	g.mu.Unlock()
	g.loadDiskCache()
}

// Startup revalidation delays. The on-disk graph is always served immediately;
// the `go list ./...` refresh is deferred past the initial burst of file opens
// so it never competes with type-checking. When a module file changed the
// refresh runs sooner to pick up the new dependency graph; otherwise it is only
// a low-priority safety net that catches source/package changes made between
// sessions (e.g. a git pull while the editor was closed).
const (
	staleRevalidateDelay = 15 * time.Second
	freshRevalidateDelay = 120 * time.Second
)

// graphFresh reports whether the on-disk graph cache's dependency set is still
// current: true when no module-structural input is at-or-newer than the cache
// file. It only decides how urgently to revalidate, not whether to: a fresh
// result merely defers the refresh longer. An equal mtime counts as not-fresh,
// so an edit racing the cache write is never missed.
func graphFresh(cacheFile, root string) bool {
	if root == "" {
		return false
	}
	fi, err := os.Stat(cacheFile)
	if err != nil {
		return false
	}
	cacheT := fi.ModTime()
	for _, f := range moduleFiles(root) {
		if s, err := os.Stat(f); err == nil && !s.ModTime().Before(cacheT) {
			return false
		}
	}
	return true
}

// moduleFiles lists the module-structural files whose modification implies the
// dependency graph may have changed: the root go.mod/go.sum/go.work/go.work.sum,
// plus the go.mod/go.sum of every module a go.work `use` directive points to.
// Without this, editing a sub-module's go.mod in a multi-module workspace would
// not touch the root files and the cache would be wrongly considered fresh.
// A missing or malformed go.work falls back to the root files only.
func moduleFiles(root string) []string {
	files := []string{
		filepath.Join(root, "go.mod"),
		filepath.Join(root, "go.sum"),
		filepath.Join(root, "go.work"),
		filepath.Join(root, "go.work.sum"),
	}
	workPath := filepath.Join(root, "go.work")
	data, err := os.ReadFile(workPath) //nolint:gosec // workPath is built from the workspace root
	if err != nil {
		return files // no go.work (the common case) or unreadable
	}
	wf, err := modfile.ParseWork(workPath, data, nil)
	if err != nil {
		return files // malformed: be conservative, root files only
	}
	for _, u := range wf.Use {
		dir := u.Path
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		files = append(files, filepath.Join(dir, "go.mod"), filepath.Join(dir, "go.sum"))
	}
	return files
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
	root := g.root
	g.mu.Unlock()
	resp, dir, ok := retargetGraph(&saved, root)
	if !ok {
		g.log.Printf("driver: disk cache dir %s not under its root %s, ignoring cache", saved.Dir, saved.Root)
		return
	}
	if dir != saved.Dir {
		g.log.Printf("driver: disk cache retargeted from %s to %s", saved.Dir, dir)
	}
	g.mu.Lock()
	g.main.resp = resp
	g.main.patternsKey = saved.PatternsKey
	g.main.patterns = saved.Patterns
	g.main.dir = dir
	g.main.stale = false
	g.mu.Unlock()
	// Decode the embed footprint off the critical path: the first workspace
	// query only needs g.main.resp, which is already published above.
	go g.setEmbedFromResp(resp)
	g.log.Printf("driver: loaded disk cache (%d bytes) from %s", len(resp), g.cacheFile)

	// Serve the cached graph immediately and revalidate in the background, but
	// DEFER the rebuild past the initial burst of file opens so the ~12s
	// `go list ./...` never competes with type-checking. graphFresh only picks
	// how long to wait; the refresh always runs, so source/package changes made
	// between sessions are still picked up.
	delay := staleRevalidateDelay
	if graphFresh(g.cacheFile, root) {
		delay = freshRevalidateDelay
	}
	g.log.Printf("driver: disk cache served; background revalidation in %s", delay)
	patterns, key := saved.Patterns, saved.PatternsKey
	time.AfterFunc(delay, func() {
		g.mu.Lock()
		if g.main.building {
			g.mu.Unlock()
			return // a MarkStale-triggered rebuild already covered it
		}
		g.main.building = true
		g.mu.Unlock()
		g.build(patterns, dir, key)
	})
}

// retargetGraph adapts a saved graph to the current workspace root. Worktrees
// of the same repository share one cache file (graphCacheKey uses the git
// common dir), but the DriverResponse inside holds absolute paths of the
// checkout that built it — served verbatim in another worktree, gopls would
// find no package for any open file ("no package metadata"). When the saved
// root differs from the current one, the paths and the build dir are rewritten
// to the current root; branch drift between checkouts is then picked up by the
// deferred revalidation, which rebuilds against the returned dir. Returns
// ok=false when the saved dir does not sit inside the saved root, in which
// case the cache cannot be retargeted and must be ignored.
func retargetGraph(saved *savedGraph, root string) (resp []byte, dir string, ok bool) {
	oldRoot := saved.Root
	if oldRoot == "" {
		oldRoot = saved.Dir // cache written before Root was recorded
	}
	if root == "" || oldRoot == root {
		return saved.Resp, saved.Dir, true
	}
	switch {
	case saved.Dir == oldRoot:
		dir = root
	case strings.HasPrefix(saved.Dir, oldRoot+string(filepath.Separator)):
		dir = root + saved.Dir[len(oldRoot):]
	default:
		return nil, "", false
	}
	return rewriteRoot(saved.Resp, oldRoot, root), dir, true
}

// rewriteRoot replaces oldRoot with newRoot in every absolute path inside a
// marshaled DriverResponse. Matches are anchored on the JSON string opening
// quote and either a path separator or closing quote, so a sibling directory
// sharing the prefix (repo vs repo-copy) and paths outside the root (the
// module cache, GOROOT) are never touched.
func rewriteRoot(resp []byte, oldRoot, newRoot string) []byte {
	resp = bytes.ReplaceAll(resp, []byte(`"`+oldRoot+`/`), []byte(`"`+newRoot+`/`))
	return bytes.ReplaceAll(resp, []byte(`"`+oldRoot+`"`), []byte(`"`+newRoot+`"`))
}

// setEmbedFromResp records the //go:embed footprint from a marshaled
// DriverResponse. Used on the disk-cache load path, where the packages are only
// available as JSON.
func (g *graphServer) setEmbedFromResp(resp []byte) {
	var r struct {
		Packages []struct {
			GoFiles       []string
			EmbedFiles    []string
			EmbedPatterns []string
		}
	}
	if json.Unmarshal(resp, &r) != nil {
		return
	}
	files := make(map[string]bool)
	prefixSet := make(map[string]bool)
	for _, p := range r.Packages {
		addEmbed(files, prefixSet, p.GoFiles, p.EmbedFiles, p.EmbedPatterns)
	}
	g.storeEmbed(files, prefixSet)
}

// setEmbedFromPackagesSlot records a slot's //go:embed footprint directly
// from the loaded packages, so a fresh build does not re-decode the
// multi-MB response it just produced. Parameterized by slot so a subslot's
// build also records its own footprint, not the main slot's.
func (g *graphServer) setEmbedFromPackagesSlot(slot *graphSlot, pkgs []*packages.Package) {
	files := make(map[string]bool)
	prefixSet := make(map[string]bool)
	for _, p := range pkgs {
		addEmbed(files, prefixSet, p.GoFiles, p.EmbedFiles, p.EmbedPatterns)
	}
	g.storeEmbedSlot(slot, files, prefixSet)
}

// addEmbed folds one package's embed footprint into the accumulating sets: the
// exact embedded files (slash-normalized) plus the literal root of every embed
// pattern, so a newly added file matching an existing pattern is still caught
// without invalidating the package's whole directory tree.
func addEmbed(files, prefixSet map[string]bool, goFiles, embedFiles, embedPatterns []string) {
	for _, f := range embedFiles {
		files[filepath.ToSlash(f)] = true
	}
	if len(embedPatterns) == 0 || len(goFiles) == 0 {
		return
	}
	dir := filepath.ToSlash(filepath.Dir(goFiles[0]))
	for _, pat := range embedPatterns {
		if root := embedLiteralRoot(pat, dir); root != "" {
			prefixSet[root] = true
		}
	}
}

// storeEmbed publishes the main slot's embed footprint. Thin wrapper over
// storeEmbedSlot, kept so setEmbedFromResp's call site is unchanged.
func (g *graphServer) storeEmbed(files, prefixSet map[string]bool) {
	g.storeEmbedSlot(&g.main, files, prefixSet)
}

func (g *graphServer) storeEmbedSlot(slot *graphSlot, files, prefixSet map[string]bool) {
	prefixes := make([]string, 0, len(prefixSet))
	for p := range prefixSet {
		prefixes = append(prefixes, p)
	}
	g.mu.Lock()
	slot.embedFiles = files
	slot.embedPrefixes = prefixes
	slot.embedReady = true
	g.mu.Unlock()
}

// embedLiteralRoot returns the fixed (wildcard-free) leading path of an embed
// pattern, resolved to an absolute slash path against the package dir. A change
// at or under this root may add a file the pattern matches; anything outside it
// cannot. E.g. "tmpl/*.html" -> "<dir>/tmpl", "config.yml" -> "<dir>/config.yml".
// Returns "" when the pattern's first segment is itself a wildcard (the root
// would be the package dir, handled by the exact-file set instead).
func embedLiteralRoot(pattern, dir string) string {
	p := strings.TrimPrefix(pattern, "all:")
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.ToSlash(p)
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		s := strings.LastIndex(p[:i], "/")
		if s < 0 {
			return ""
		}
		p = p[:s]
	}
	return strings.TrimRight(p, "/")
}

// IsEmbedFile reports whether a non-Go path can affect the package graph as an
// //go:embed asset: it is a currently embedded file, or it sits at/under the
// literal root of some embed pattern. Until the footprint is known (cache still
// loading) it returns true so a possible embed change is never missed.
func (g *graphServer) IsEmbedFile(path string) bool {
	p := filepath.ToSlash(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.main.embedReady {
		return true
	}
	if g.main.embedFiles[p] {
		return true
	}
	for _, root := range g.main.embedPrefixes {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// saveDiskCache writes the current graph to the on-disk cache file.
// Callers must NOT hold g.mu.
func (g *graphServer) saveDiskCache(resp []byte, patternsKey string, patterns []string, dir string) {
	if g.cacheFile == "" {
		return
	}
	g.mu.Lock()
	root := g.root
	g.mu.Unlock()
	saved := savedGraph{
		Resp:        resp,
		PatternsKey: patternsKey,
		Patterns:    patterns,
		Dir:         dir,
		Root:        root,
	}
	b, err := json.Marshal(saved)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.cacheFile), 0o750); err != nil {
		return
	}
	tmp := g.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
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

// answer dispatches a driver query to the slot owning it: the main workspace
// slot, or a nested module's own subslot (e.g. a git worktree with its own
// go.mod). The dispatch runs BEFORE any cache-hit check, which matters:
// dirCompatible alone is a string-prefix test, so a nested module's Dir would
// otherwise wrongly prefix-match the main slot's cached dir (which, right
// after startup, is often the workspace root itself — every subdirectory
// string-prefix-matches that) and be served the wrong, main-workspace graph.
//
// A nested module's subslot answers differently from the main slot
// (answerNestedSlot, not answerSlot): see resolveModRootForQuery for why.
func (g *graphServer) answer(q driverQuery) []byte {
	var req packages.DriverRequest
	if err := json.Unmarshal(q.Request, &req); err != nil {
		return notHandled
	}
	key := strings.Join(q.Patterns, "\x00")

	modRoot := g.resolveModRootForQuery(q)
	if modRoot == "" {
		return g.answerSlot(&g.main, g.idx, q, req, key, true)
	}
	slot, idx := g.subslotFor(modRoot)
	return g.answerNestedSlot(slot, idx, modRoot, q, req)
}

// resolveModRoot resolves which module owns a driver query's directory: ""
// for the main module, or a nested module's own root otherwise. Defensive:
// a graphServer with no modRoots wiring (e.g. one built directly by an
// existing test) or a query with no usable Dir always resolves to the main
// module, matching answer's pre-subslot behavior byte-for-byte.
func (g *graphServer) resolveModRoot(dir string) string {
	if g.modRoots == nil || dir == "" {
		return ""
	}
	g.mu.Lock()
	root := g.root
	g.mu.Unlock()
	if root == "" {
		return ""
	}
	return g.modRoots.RootFor(dir, root)
}

// resolveModRootForQuery resolves which nested module a driver query belongs
// to, like resolveModRoot, but also inspects the query's own "file=" pattern
// targets when Dir does not resolve to one.
//
// gopls, once a GOPACKAGESDRIVER is configured, loads the whole workspace
// through a single view rooted at the workspace folder — it does not create
// gopls's usual per-module zero-config view for a nested go.mod the way it
// does without a driver — and it invokes the driver with Dir always set to
// that one view's root, even for a query whose target file lives inside a
// nested module's own go.mod boundary. So Dir alone cannot be trusted to
// route a nested module's queries to its own subslot; the "file=" pattern
// arguments, which name the actual target file, are the only reliable signal
// in that case.
func (g *graphServer) resolveModRootForQuery(q driverQuery) string {
	if modRoot := g.resolveModRoot(q.Dir); modRoot != "" {
		return modRoot
	}
	for _, f := range queryFileTargets(q.Patterns) {
		if modRoot := g.resolveModRoot(filepath.Dir(f)); modRoot != "" {
			return modRoot
		}
	}
	return ""
}

// queryFileTargets extracts the absolute file paths named by "file=" driver
// query patterns (see golang.org/x/tools/go/packages' query pattern syntax).
func queryFileTargets(patterns []string) []string {
	var files []string
	for _, p := range patterns {
		if f, ok := strings.CutPrefix(p, "file="); ok {
			files = append(files, f)
		}
	}
	return files
}

// subslotFor returns the lazily-created subslot and owning-module index for
// a nested module root. A second call for the same modRoot returns the same
// slot.
func (g *graphServer) subslotFor(modRoot string) (*graphSlot, *revIndex) {
	g.mu.Lock()
	if g.subslots == nil {
		g.subslots = map[string]*graphSlot{}
	}
	slot, ok := g.subslots[modRoot]
	if !ok {
		slot = &graphSlot{}
		g.subslots[modRoot] = slot
	}
	g.mu.Unlock()
	var idx *revIndex
	if g.indexFor != nil {
		idx = g.indexFor(modRoot)
	}
	return slot, idx
}

// answerSlot runs the cache-hit/miss, dir-mismatch, staleness and
// overlay-dirty dispatch logic against one slot. idx is the reverse-import
// index used by overlayDirty to check unsaved-overlay import changes; it may
// be nil (see overlayDirty). persist controls whether a build triggered from
// this slot also writes the on-disk cache: true for the main slot only — a
// subslot's disk-cache key (the git common dir) is shared with the main
// slot's, so writing there would corrupt it.
func (g *graphServer) answerSlot(slot *graphSlot, idx *revIndex, q driverQuery, req packages.DriverRequest, key string, persist bool) []byte {
	g.mu.Lock()
	resp := slot.resp
	stale := slot.stale
	hasCache := resp != nil && key == slot.patternsKey

	// A query from outside the cached dir targets another checkout; the cached
	// absolute paths would be wrong there. Fall back to go list and rebuild for
	// the queried dir so serving resumes from the right root.
	if hasCache && !dirCompatible(q.Dir, slot.dir) {
		if isWorkspaceQuery(q.Patterns) && !slot.building {
			slot.building = true
			patterns := append([]string(nil), q.Patterns...)
			dir := q.Dir
			go g.buildSlot(slot, patterns, dir, key, persist)
		}
		cached := slot.dir
		g.mu.Unlock()
		g.log.Printf("driver: NotHandled (query dir %s does not match cached dir %s)", q.Dir, cached)
		return notHandled
	}

	if !hasCache {
		// No cache at all: trigger a background build for workspace queries
		// and tell gopls to fall back to the real go list.
		if isWorkspaceQuery(q.Patterns) && !slot.building {
			slot.building = true
			patterns := append([]string(nil), q.Patterns...)
			dir := q.Dir
			go g.buildSlot(slot, patterns, dir, key, persist)
		}
		g.mu.Unlock()
		g.log.Printf("driver: NotHandled (no cache, patterns=%v)", q.Patterns)
		return notHandled
	}

	// We have a cache. If it is stale (go.mod / imports changed on disk),
	// kick off a background rebuild but still serve the cached data so
	// re-scopes during the ~13s rebuild window don't regress to full go list.
	if stale && !slot.building {
		slot.building = true
		patterns, dir := slot.patterns, slot.dir
		go g.buildSlot(slot, patterns, dir, key, persist)
	}
	g.mu.Unlock()

	// Only fall back for live import changes the user has in an unsaved
	// overlay — those modify the package graph in a way the cached snapshot
	// cannot reflect.
	if g.overlayDirty(idx, req.Overlay) {
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

// nestedModuleCacheKey is the fixed patternsKey every subslot build is
// stored under, regardless of which literal pattern the query that triggered
// it asked for. A subslot always answers with the WHOLE nested module's
// package graph (built from the "all" pattern, see nestedModuleLoadPattern):
// nested modules are small (a git worktree is a tiny slice of the
// monorepo), so loading the whole thing once is cheap and lets every
// subsequent query for that module — whatever its own pattern — hit the same
// cache, instead of rebuilding per distinct pattern.
const nestedModuleCacheKey = "nested:all"

// nestedModuleLoadPattern is the pattern every subslot build uses, regardless
// of the query pattern that triggered it — see nestedModuleCacheKey. "all"
// (every package in the module rooted at the build dir, plus its
// dependencies) rather than "./..." because a driver query's own build dir is
// always the nested module's own root (see resolveModRootForQuery), so the
// two are equivalent for this purpose.
var nestedModuleLoadPattern = []string{"all"}

// answerNestedSlot answers a query dispatched to a nested module's subslot.
//
// Unlike answerSlot (the main slot), a cache miss here cannot defer to
// NotHandled and trust gopls's own native `go list` fallback: gopls, in
// GOPACKAGESDRIVER mode, keeps a single view rooted at the workspace folder
// and runs that fallback from there too, so it can never resolve a file
// belonging to a different module's go.mod (see resolveModRootForQuery). A
// synchronous build is the only way to answer such a query correctly;
// acceptable because nested modules are small.
func (g *graphServer) answerNestedSlot(slot *graphSlot, idx *revIndex, modRoot string, q driverQuery, req packages.DriverRequest) []byte {
	g.mu.Lock()
	resp := slot.resp
	stale := slot.stale
	hasCache := resp != nil
	g.mu.Unlock()

	if hasCache {
		return g.answerNestedSlotFromCache(slot, idx, modRoot, q, req, resp, stale)
	}

	g.mu.Lock()
	building := slot.building
	if !building {
		slot.building = true
	}
	g.mu.Unlock()
	if building {
		g.log.Printf("driver: nested module %s build already in progress; NotHandled", modRoot)
		return notHandled
	}

	g.buildSlot(slot, nestedModuleLoadPattern, modRoot, nestedModuleCacheKey, false)

	g.mu.Lock()
	resp = slot.resp
	g.mu.Unlock()
	if resp == nil {
		g.log.Printf("driver: nested module %s build failed; NotHandled", modRoot)
		return notHandled
	}
	return resp
}

// answerNestedSlotFromCache answers a nested-module subslot query that
// already has a cached graph: fresh cache is served as-is; a dirty overlay
// falls back to NotHandled (the cached snapshot cannot reflect unsaved
// import changes); a stale cache (go.mod / imports changed on disk) is still
// served immediately, with a background rebuild kicked off at most once.
func (g *graphServer) answerNestedSlotFromCache(slot *graphSlot, idx *revIndex, modRoot string, q driverQuery, req packages.DriverRequest, resp []byte, stale bool) []byte {
	if g.overlayDirty(idx, req.Overlay) {
		g.log.Printf("driver: nested module %s overlay changes imports, falling back to go list", modRoot)
		return notHandled
	}
	if !stale {
		g.log.Printf("driver: served %d patterns from nested-module cache (module %s, %d bytes)", len(q.Patterns), modRoot, len(resp))
		return resp
	}
	g.mu.Lock()
	building := slot.building
	if !building {
		slot.building = true
	}
	g.mu.Unlock()
	if !building {
		go g.buildSlot(slot, nestedModuleLoadPattern, modRoot, nestedModuleCacheKey, false)
	}
	g.log.Printf("driver: served %d patterns from stale nested-module cache (module %s, rebuild in progress)", len(q.Patterns), modRoot)
	return resp
}

// dirCompatible reports whether a driver query issued from dir can be served
// by a graph rooted at cachedDir: the same dir or a subdirectory of it.
// Anything else means the query targets another checkout. Empty values (an
// unset root, a query without a dir) are treated as compatible so the guard
// never regresses the plain single-checkout path.
func dirCompatible(dir, cachedDir string) bool {
	if dir == "" || cachedDir == "" || dir == cachedDir {
		return true
	}
	return strings.HasPrefix(dir, cachedDir+string(filepath.Separator))
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

// build runs `go list` for patterns/dir and populates the main slot with the
// result, persisting it to the on-disk cache. Thin wrapper kept for its
// existing callers (loadDiskCache, MarkStale); see buildSlot for the
// slot-parameterized logic subslots also use.
func (g *graphServer) build(patterns []string, dir, key string) {
	g.buildSlot(&g.main, patterns, dir, key, true)
}

// buildSlot runs `go list` for patterns/dir and populates slot with the
// result. persist controls whether the build is also written to the on-disk
// cache (main slot only, see answerSlot).
func (g *graphServer) buildSlot(slot *graphSlot, patterns []string, dir, key string, persist bool) {
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
		slot.building = false
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
		slot.building = false
		g.mu.Unlock()
		return
	}
	g.mu.Lock()
	slot.resp = b
	slot.patterns = patterns
	slot.patternsKey = key
	slot.dir = dir
	slot.stale = false
	slot.building = false
	g.mu.Unlock()
	g.setEmbedFromPackagesSlot(slot, all)
	g.log.Printf("driver: graph built in %s (%d packages, %d roots, %dMB)",
		time.Since(start).Round(time.Millisecond), len(all), len(rootIDs), len(b)>>20)
	if persist {
		go g.saveDiskCache(b, key, patterns, dir)
	}
}

// overlayDirty reports whether any open-file overlay changes a file's import
// set compared to the on-disk state the cache was built from, per idx (the
// reverse-import index owning the slot being checked). A nil idx (no index
// wired for this slot, e.g. a nested module whose index has not been built)
// is conservative: any non-empty overlay is treated as dirty.
func (g *graphServer) overlayDirty(idx *revIndex, overlay map[string][]byte) bool {
	for path, content := range overlay {
		if !strings.HasSuffix(path, ".go") {
			return true
		}
		if idx == nil || !idx.SameImports(path, content) {
			return true
		}
	}
	return false
}

// MarkStale schedules a background rebuild of the main slot; until it
// finishes, queries fall back to the real go list.
func (g *graphServer) MarkStale(reason string) {
	g.markSlotStale(&g.main, reason, true)
}

// MarkStaleFor marks stale the slot owning path's module: the main
// workspace slot when path is not under any nested module, or that
// module's own subslot otherwise. Unlike MarkStale, it never creates a
// subslot that does not already exist — a module nothing has ever been
// built for has no cache to invalidate.
func (g *graphServer) MarkStaleFor(path, reason string) {
	modRoot := g.resolveModRoot(filepath.Dir(path))
	if modRoot == "" {
		g.MarkStale(reason)
		return
	}
	g.mu.Lock()
	slot := g.subslots[modRoot]
	g.mu.Unlock()
	if slot == nil {
		return
	}
	g.markSlotStale(slot, reason, false)
}

// markSlotStale is the shared MarkStale/MarkStaleFor implementation,
// parameterized by slot and by whether a triggered rebuild persists to the
// on-disk cache (main slot only, see answerSlot).
func (g *graphServer) markSlotStale(slot *graphSlot, reason string, persist bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if slot.patternsKey == "" {
		return // never built; nothing to refresh
	}
	if !slot.stale {
		g.log.Printf("driver: cache marked stale (%s)", reason)
	}
	slot.stale = true
	if slot.rebuildTimer != nil {
		slot.rebuildTimer.Stop()
	}
	slot.rebuildTimer = time.AfterFunc(3*time.Second, func() {
		g.mu.Lock()
		if slot.building {
			g.mu.Unlock()
			return
		}
		slot.building = true
		patterns, dir, key := slot.patterns, slot.dir, slot.patternsKey
		g.mu.Unlock()
		g.buildSlot(slot, patterns, dir, key, persist)
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
