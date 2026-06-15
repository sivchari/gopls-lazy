package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// filtersLocked returns the directoryFilters for the current scope. The
// caller must hold p.mu. During a temporary whole-workspace widening it
// returns the editor's own filters (or the gopls default): the key must be
// SET explicitly, because gopls layers workspace/configuration over
// initializationOptions and an absent key would keep the old value.
func (p *proxy) filtersLocked() []string {
	if !p.fullUntil.IsZero() {
		if len(p.userFilters) > 0 {
			return p.userFilters
		}
		return []string{"-**/node_modules"}
	}
	dirs := make([]string, 0, len(p.scope))
	for d := range p.scope {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	// If any open file lives at the workspace root (unit "."), gopls has no
	// filter pattern that matches the root directory alone. Fall back to no
	// filters so gopls loads the full workspace. This is correct for small
	// repos (like gopls-fleet itself) and acceptable for large repos where
	// root-level Go files are uncommon.
	for _, d := range dirs {
		if d == "." {
			if len(p.userFilters) > 0 {
				return p.userFilters
			}
			return []string{}
		}
	}

	fs := []string{"-**"}
	for _, d := range dirs {
		fs = append(fs, "+"+d)
	}
	return fs
}

// setFilters applies the proxy's filters to a gopls settings object.
func setFilters(settings map[string]any, filters []string) {
	settings["directoryFilters"] = filters
}

// unitFor maps an absolute file path to its scope unit relative to the
// workspace root.  Files that live directly in the workspace root are mapped
// to the special unit "." (the root).
func (p *proxy) unitFor(path string) (string, bool) {
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	if root == "" || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", false
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	if rel == "." {
		// File is directly in the workspace root (e.g. main.go in a small
		// single-package repo). Represent it as the root unit ".".
		return ".", true
	}
	return scopeUnit(filepath.ToSlash(rel), p.opts.granularity), true
}

// pushScope tells gopls that configuration changed; gopls then re-requests
// workspace/configuration, and the patched answer carries the new filters.
// The new scope is also persisted to disk so the next session can restore it.
func (p *proxy) pushScope() {
	note := message{
		JSONRPC: "2.0",
		Method:  "workspace/didChangeConfiguration",
		Params:  json.RawMessage(`{"settings":{}}`),
	}
	raw, err := json.Marshal(note)
	if err != nil {
		return
	}
	p.mu.Lock()
	if !p.fullUntil.IsZero() {
		p.log.Printf("rescope: full workspace via %v (until %s)", p.filtersLocked(), p.fullUntil.Format("15:04:05"))
	} else {
		p.log.Printf("rescope: filters=%v", p.filtersLocked())
	}
	p.mu.Unlock()
	p.toServer.write(raw)
	go p.saveScope()
}

// scopeUnit truncates a relative directory to at most n leading segments,
// so deep files map to their service root (go/services/auth/...).
func scopeUnit(rel string, n int) string {
	segs := strings.Split(rel, "/")
	if len(segs) > n {
		segs = segs[:n]
	}
	return strings.Join(segs, "/")
}

// ---- scope persistence -------------------------------------------------------
//
// The active scope (set of directory units) is saved to disk after each
// rescope. On the next startup the proxy restores it as the initial
// directoryFilters so gopls starts loading those packages before the user
// even opens a file. This eliminates the "module '.' not in workspace" orphan
// error the user would otherwise see during the type-check warmup window.

type savedScope struct {
	Units []string `json:"units"`
}

// scopeCacheFile returns the path for the on-disk scope file, using the same
// root-based key as the graph cache so worktrees share state.
func scopeCacheFile(root string) string {
	cf := graphCacheFile(root)
	dir, base := filepath.Split(cf)
	// Replace "graph-<hash>.json" with "scope-<hash>.json".
	return filepath.Join(dir, "scope-"+strings.TrimPrefix(base, "graph-"))
}

// saveScope writes the current scope units to disk. Called in a goroutine
// from pushScope. Callers must NOT hold p.mu.
func (p *proxy) saveScope() {
	p.mu.Lock()
	root := p.root
	units := make([]string, 0, len(p.scope))
	for u := range p.scope {
		units = append(units, u)
	}
	p.mu.Unlock()
	if root == "" || len(units) == 0 {
		return
	}
	path := scopeCacheFile(root)
	b, err := json.Marshal(savedScope{Units: units})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

// restoreScope loads the previously saved scope into p.scope so the initial
// directoryFilters include the services the user was editing last session.
// Must be called before patchInitialize sends the first filters to gopls.
// Callers must NOT hold p.mu.
func (p *proxy) restoreScope(root string) {
	path := scopeCacheFile(root)
	data, err := os.ReadFile(path)
	if err != nil {
		return // first run or no saved scope
	}
	var saved savedScope
	if err := json.Unmarshal(data, &saved); err != nil || len(saved.Units) == 0 {
		return
	}
	p.mu.Lock()
	for _, u := range saved.Units {
		if _, exists := p.scope[u]; !exists {
			p.scope[u] = &scopeEntry{open: map[string]bool{}, lastActive: time.Now()}
		}
	}
	p.mu.Unlock()
	p.log.Printf("scope restored from previous session: %v", saved.Units)
}
