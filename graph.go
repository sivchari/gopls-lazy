package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
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
type graphServer struct {
	idx      *revIndex
	log      *log.Logger
	sockPath string
	ln       net.Listener

	mu           sync.Mutex
	resp         []byte // cached marshaled DriverResponse
	patternsKey  string
	patterns     []string
	dir          string
	building     bool
	stale        bool
	rebuildTimer *time.Timer
}

type driverQuery struct {
	Patterns []string
	Dir      string
	Request  json.RawMessage
}

func startGraphServer(idx *revIndex, logger *log.Logger) (*graphServer, error) {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("gopls-fleet-%d.sock", os.Getpid()))
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

func (g *graphServer) handle(conn net.Conn) {
	defer conn.Close()
	var q driverQuery
	if err := json.NewDecoder(conn).Decode(&q); err != nil {
		return
	}
	conn.Write(g.answer(q))
}

var notHandled = []byte(`{"NotHandled":true}`)

func (g *graphServer) answer(q driverQuery) []byte {
	var req packages.DriverRequest
	if err := json.Unmarshal(q.Request, &req); err != nil {
		return notHandled
	}
	key := strings.Join(q.Patterns, "\x00")

	g.mu.Lock()
	if g.resp != nil && !g.stale && key == g.patternsKey {
		resp := g.resp
		g.mu.Unlock()
		if g.overlayDirty(req.Overlay) {
			g.log.Printf("driver: overlay changes imports, falling back to go list")
			return notHandled
		}
		g.log.Printf("driver: served %d patterns from cache (%d bytes)", len(q.Patterns), len(resp))
		return resp
	}
	// Cache miss. If this is a full workspace query, learn its exact
	// patterns and build the cache in the background for next time.
	if isWorkspaceQuery(q.Patterns) && !g.building {
		g.building = true
		patterns := append([]string(nil), q.Patterns...)
		dir := q.Dir
		go g.build(patterns, dir, key)
	}
	g.mu.Unlock()
	g.log.Printf("driver: NotHandled (patterns=%v stale=%v)", q.Patterns, g.stale)
	return notHandled
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
