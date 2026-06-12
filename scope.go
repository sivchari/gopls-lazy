package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

func (p *proxy) filters() []string {
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
	return scopeUnit(filepath.ToSlash(rel), p.granularity), true
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
	p.log.Printf("rescope: filters=%v", p.filters())
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
