package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
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
// workspace root.
func (p *proxy) unitFor(path string) (string, bool) {
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	if root == "" || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", false
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return scopeUnit(filepath.ToSlash(rel), p.opts.granularity), true
}

// pushScope tells gopls that configuration changed; gopls then re-requests
// workspace/configuration, and the patched answer carries the new filters.
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
