package goplslazy

import (
	"go/parser"
	"go/token"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// revIndex is a reverse-import index over the workspace's own packages,
// built by parsing only import clauses (fast even for tens of thousands of
// files). It answers "which directories (transitively) import package X",
// which is exactly the set a rename/references request must have in scope.
type revIndex struct {
	mu          sync.RWMutex
	root        string
	modPath     string
	fileImports map[string][]string        // rel file -> imported rel pkg dirs (internal only)
	fileSymbols map[string][]indexedSymbol // rel file -> top-level symbols in that file
	importers   map[string]map[string]bool // rel pkg dir -> set of rel dirs importing it
	readyCh     chan struct{}
	readyOnce   sync.Once
	log         *log.Logger
}

func newRevIndex(logger *log.Logger) *revIndex {
	return &revIndex{
		fileImports: map[string][]string{},
		fileSymbols: map[string][]indexedSymbol{},
		importers:   map[string]map[string]bool{},
		readyCh:     make(chan struct{}),
		log:         logger,
	}
}

func (ri *revIndex) Ready() bool {
	select {
	case <-ri.readyCh:
		return true
	default:
		return false
	}
}

func (ri *revIndex) WaitReady(d time.Duration) bool {
	select {
	case <-ri.readyCh:
		return true
	case <-time.After(d):
		return false
	}
}

// Build scans the workspace once. Safe to call from a goroutine.
func (ri *revIndex) Build(root string) {
	start := time.Now()
	modPath := modulePath(filepath.Join(root, "go.mod"))
	ri.mu.Lock()
	ri.root = root
	ri.modPath = modPath
	ri.mu.Unlock()

	var files []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // swallow WalkDir entry errors to continue indexing
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})

	var wg sync.WaitGroup
	ch := make(chan string, 256)
	for range runtime.NumCPU() {
		wg.Go(func() {
			for path := range ch {
				ri.UpdateFile(path)
			}
		})
	}
	for _, f := range files {
		ch <- f
	}
	close(ch)
	wg.Wait()

	ri.readyOnce.Do(func() { close(ri.readyCh) })
	ri.log.Printf("revindex: %d files indexed in %s (module %s)", len(files), time.Since(start).Round(time.Millisecond), modPath)
}

// UpdateFile (re-)parses one file's imports and merges its edges into the
// index. Edges are only added, never removed: a stale extra edge can only
// widen a scope expansion, which is safe. Returns true if the file's import
// set changed.
func (ri *revIndex) UpdateFile(path string) bool {
	ri.mu.RLock()
	root, modPath := ri.root, ri.modPath
	ri.mu.RUnlock()
	if root == "" || modPath == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	rel = filepath.ToSlash(rel)

	src, err := os.ReadFile(path) //nolint:gosec // path is validated to be inside the workspace root via filepath.Rel
	if err != nil {
		// Deleted: keep import edges (additive index), but drop symbols from
		// this file so workspace/symbol does not report stale definitions.
		ri.mu.Lock()
		delete(ri.fileSymbols, rel)
		ri.mu.Unlock()
		return true
	}
	all, internal, symbols := parseFileMetadata(src, modPath, rel)
	dir := filepath.ToSlash(filepath.Dir(rel))

	ri.mu.Lock()
	defer ri.mu.Unlock()
	old := ri.fileImports[rel]
	changed := !equalStrings(old, all)
	ri.fileImports[rel] = all
	ri.fileSymbols[rel] = symbols
	for _, imp := range internal {
		set := ri.importers[imp]
		if set == nil {
			set = map[string]bool{}
			ri.importers[imp] = set
		}
		set[dir] = true
	}
	return changed
}

// SameImports reports whether the given (possibly unsaved) content has the
// same import set as the file had when it was last indexed from disk.
func (ri *revIndex) SameImports(path string, content []byte) bool {
	ri.mu.RLock()
	root, modPath := ri.root, ri.modPath
	ri.mu.RUnlock()
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	all, _ := parseImports(content, modPath)
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return equalStrings(ri.fileImports[filepath.ToSlash(rel)], all)
}

// ClosureUnits returns the scope units of every directory that transitively
// imports the package in relDir, plus relDir's own unit.
func (ri *revIndex) ClosureUnits(relDir string, granularity int) []string {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	seen := map[string]bool{relDir: true}
	queue := []string{relDir}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		for imp := range ri.importers[d] {
			if !seen[imp] {
				seen[imp] = true
				queue = append(queue, imp)
			}
		}
	}
	units := map[string]bool{}
	for d := range seen {
		units[scopeUnit(d, granularity)] = true
	}
	out := make([]string, 0, len(units))
	for u := range units {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// parseImports extracts the metadata-relevant signature of a Go source
// file: all import paths plus //go:embed directives (both change the
// package graph), and the subset of imports that refers to
// workspace-internal packages (as rel dirs).
func parseImports(src []byte, modPath string) (all, internal []string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.ImportsOnly)
	if err != nil || f == nil {
		return nil, nil
	}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		all = append(all, p)
		if modPath == "" {
			continue
		}
		if p == modPath {
			internal = append(internal, ".")
		} else if strings.HasPrefix(p, modPath+"/") {
			internal = append(internal, p[len(modPath)+1:])
		}
	}
	// ImportsOnly stops at the import block, so scan the raw source for
	// embed directives. The marker prefix cannot collide with import paths.
	for line := range strings.Lines(string(src)) {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//go:embed") {
			all = append(all, "\x00embed:"+t)
		}
	}
	sort.Strings(all)
	sort.Strings(internal)
	return all, internal
}

func parseFileMetadata(src []byte, modPath, rel string) (all, internal []string, symbols []indexedSymbol) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil && f == nil {
		return nil, nil, nil
	}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		all = append(all, p)
		if modPath == "" {
			continue
		}
		if p == modPath {
			internal = append(internal, ".")
		} else if strings.HasPrefix(p, modPath+"/") {
			internal = append(internal, p[len(modPath)+1:])
		}
	}
	for line := range strings.Lines(string(src)) {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//go:embed") {
			all = append(all, "\x00embed:"+t)
		}
	}
	symbols = collectSymbols(fset, f, rel)
	sort.Strings(all)
	sort.Strings(internal)
	return all, internal, symbols
}

func modulePath(gomod string) string {
	data, err := os.ReadFile(gomod) //nolint:gosec // gomod path is constructed by walking the workspace root
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
