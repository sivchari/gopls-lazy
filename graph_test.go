package goplslazy

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGoMinor(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"go1.26.4", 26},
		{"go1.27-devel_69a99fdcbb Sun May 17", 27},
		{"devel +abcdef", 0},
	}
	for _, tt := range tests {
		if got := goMinor(tt.in); got != tt.want {
			t.Errorf("goMinor(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestIsWorkspaceQuery(t *testing.T) {
	if !isWorkspaceQuery([]string{"/repo/...", "builtin"}) {
		t.Error("recursive pattern should be a workspace query")
	}
	if isWorkspaceQuery([]string{"file=/repo/main.go"}) {
		t.Error("file= pattern should not be a workspace query")
	}
}

func TestGraphFresh(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "graph.json")
	gomod := filepath.Join(dir, "go.mod")
	write := func(p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	chtime := func(p string, mod time.Time) {
		t.Helper()
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	write(gomod)
	write(cache)

	// go.mod older than cache -> fresh.
	chtime(gomod, time.Now().Add(-time.Hour))
	if !graphFresh(cache, dir) {
		t.Error("cache newer than go.mod should be fresh")
	}
	// go.mod newer than cache -> stale.
	chtime(gomod, time.Now().Add(time.Hour))
	if graphFresh(cache, dir) {
		t.Error("go.mod newer than cache should be stale")
	}
	// go.mod equal mtime to cache -> stale (an edit racing the cache write must
	// not be treated as fresh).
	same := time.Now()
	chtime(cache, same)
	chtime(gomod, same)
	if graphFresh(cache, dir) {
		t.Error("go.mod with the same mtime as cache should be stale")
	}
	// Missing cache file -> stale.
	if graphFresh(filepath.Join(dir, "absent.json"), dir) {
		t.Error("missing cache should be stale")
	}
	// Empty root -> stale (cannot verify).
	if graphFresh(cache, "") {
		t.Error("empty root should be stale")
	}
}

func TestGraphFresh_GoWorkSubmodule(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "graph.json")
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	chtime := func(p string, mod time.Time) {
		t.Helper()
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	subMod := filepath.Join(dir, "svc", "go.mod")
	write(filepath.Join(dir, "go.work"), "go 1.22\n\nuse (\n\t./svc\n)\n")
	write(filepath.Join(dir, "go.mod"), "module root\n")
	write(subMod, "module root/svc\n")
	write(cache, "{}")

	old := time.Now().Add(-time.Hour)
	for _, p := range []string{filepath.Join(dir, "go.work"), filepath.Join(dir, "go.mod"), subMod} {
		chtime(p, old)
	}
	// All module files older than cache -> fresh.
	if !graphFresh(cache, dir) {
		t.Error("all module files older than cache should be fresh")
	}
	// Editing only the sub-module's go.mod (root files untouched) must make the
	// cache stale -- the regression this go.work support fixes.
	chtime(subMod, time.Now().Add(time.Hour))
	if graphFresh(cache, dir) {
		t.Error("a newer go.work sub-module go.mod should make the cache stale")
	}
}

func TestEmbedLiteralRoot(t *testing.T) {
	const dir = "/repo/x"
	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{"relative wildcard in subdir", "tmpl/*.html", "/repo/x/tmpl"},
		{"relative literal file", "config.yml", "/repo/x/config.yml"},
		{"relative literal dir", "assets", "/repo/x/assets"},
		{"absolute literal file (runtime form)", "/repo/x/services.gen.yml", "/repo/x/services.gen.yml"},
		{"absolute wildcard", "/repo/x/static/*.js", "/repo/x/static"},
		{"all: prefix stripped", "all:templates", "/repo/x/templates"},
		{"trailing slash dir", "tmpl/", "/repo/x/tmpl"},
		{"pure wildcard first segment", "*.html", "/repo/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := embedLiteralRoot(tt.pattern, dir); got != tt.want {
				t.Errorf("embedLiteralRoot(%q, %q) = %q, want %q", tt.pattern, dir, got, tt.want)
			}
		})
	}
}

func TestGraphServer_IsEmbedFile(t *testing.T) {
	g := &graphServer{log: log.New(io.Discard, "", 0)}

	// Before the footprint is known, be conservative.
	if !g.IsEmbedFile("/repo/x/asset.json") {
		t.Error("unknown footprint should be conservative (true)")
	}

	resp, err := json.Marshal(map[string]any{
		"Packages": []map[string]any{
			{
				"GoFiles":       []string{"/repo/x/main.go"},
				"EmbedFiles":    []string{"/repo/x/tmpl.html"},
				"EmbedPatterns": []string{"*.html"},
			},
			{"GoFiles": []string{"/repo/y/y.go"}}, // no embed patterns
			{
				// Embeds a single specific file: a sibling file in the same
				// directory must NOT be treated as an embed asset (the bug that
				// turned one high-level embedding package into a whole-subtree
				// invalidation).
				"GoFiles":       []string{"/repo/svc/svc.go"},
				"EmbedFiles":    []string{"/repo/svc/config.gen.yml"},
				"EmbedPatterns": []string{"config.gen.yml"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.setEmbedFromResp(resp)

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"exact embed file", "/repo/x/tmpl.html", true},
		{"new file under embed-pattern dir", "/repo/x/new.html", true},
		{"file in subdir of embed-pattern dir", "/repo/x/sub/a.txt", true},
		{"package without embed patterns", "/repo/y/data.json", false},
		{"unrelated dir", "/repo/z/other.txt", false},
		{"specific-file embed, exact", "/repo/svc/config.gen.yml", true},
		{"specific-file embed, sibling not matched", "/repo/svc/other.json", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := g.IsEmbedFile(tt.path); got != tt.want {
				t.Errorf("IsEmbedFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
