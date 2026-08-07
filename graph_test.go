package goplslazy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
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

func TestRewriteRoot(t *testing.T) {
	resp := []byte(`{"GoFiles":["/old/repo/pkg/a.go"],"Dir":"/old/repo",` +
		`"Sibling":"/old/repo-copy/pkg/a.go","Dep":"/home/u/go/pkg/mod/x@v1/a.go","ID":"example.com/old/repo/pkg"}`)
	got := string(rewriteRoot(resp, "/old/repo", "/new/wt"))
	want := `{"GoFiles":["/new/wt/pkg/a.go"],"Dir":"/new/wt",` +
		`"Sibling":"/old/repo-copy/pkg/a.go","Dep":"/home/u/go/pkg/mod/x@v1/a.go","ID":"example.com/old/repo/pkg"}`
	if got != want {
		t.Errorf("rewriteRoot() = %s, want %s", got, want)
	}
}

// BenchmarkRewriteRoot measures the cost the worktree retargeting adds to a
// warm startup, on a payload the size of a production monorepo graph cache.
func BenchmarkRewriteRoot(b *testing.B) {
	var buf []byte
	buf = append(buf, `{"Packages":[`...)
	for i := 0; len(buf) < 32<<20; i++ {
		buf = append(buf, `{"GoFiles":["/old/checkout/go/services/auth/internal/tokens/file`...)
		buf = strconv.AppendInt(buf, int64(i), 10)
		buf = append(buf, `.go"],"Dir":"/old/checkout"},`...)
	}
	buf = append(buf, `{}]}`...)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		rewriteRoot(buf, "/old/checkout", "/new/worktree")
	}
}

func TestRetargetGraph(t *testing.T) {
	tests := []struct {
		name     string
		saved    savedGraph
		root     string
		wantResp string
		wantDir  string
		wantOK   bool
	}{
		{
			name:     "same root passes through",
			saved:    savedGraph{Resp: []byte(`{"f":"/repo/a.go"}`), Dir: "/repo", Root: "/repo"},
			root:     "/repo",
			wantResp: `{"f":"/repo/a.go"}`,
			wantDir:  "/repo",
			wantOK:   true,
		},
		{
			name:     "other worktree rewrites paths and dir",
			saved:    savedGraph{Resp: []byte(`{"f":"/repo/a.go"}`), Dir: "/repo", Root: "/repo"},
			root:     "/wt",
			wantResp: `{"f":"/wt/a.go"}`,
			wantDir:  "/wt",
			wantOK:   true,
		},
		{
			name:     "dir under root is remapped",
			saved:    savedGraph{Resp: []byte(`{}`), Dir: "/repo/sub", Root: "/repo"},
			root:     "/wt",
			wantResp: `{}`,
			wantDir:  "/wt/sub",
			wantOK:   true,
		},
		{
			name:     "legacy cache without root falls back to dir",
			saved:    savedGraph{Resp: []byte(`{"f":"/repo/a.go"}`), Dir: "/repo"},
			root:     "/wt",
			wantResp: `{"f":"/wt/a.go"}`,
			wantDir:  "/wt",
			wantOK:   true,
		},
		{
			name:     "empty current root passes through",
			saved:    savedGraph{Resp: []byte(`{"f":"/repo/a.go"}`), Dir: "/repo", Root: "/repo"},
			root:     "",
			wantResp: `{"f":"/repo/a.go"}`,
			wantDir:  "/repo",
			wantOK:   true,
		},
		{
			name:   "dir outside root cannot be retargeted",
			saved:  savedGraph{Resp: []byte(`{}`), Dir: "/elsewhere", Root: "/repo"},
			root:   "/wt",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, dir, ok := retargetGraph(&tt.saved, tt.root)
			if ok != tt.wantOK {
				t.Fatalf("retargetGraph() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if string(resp) != tt.wantResp {
				t.Errorf("retargetGraph() resp = %s, want %s", resp, tt.wantResp)
			}
			if dir != tt.wantDir {
				t.Errorf("retargetGraph() dir = %s, want %s", dir, tt.wantDir)
			}
		})
	}
}

func TestDirCompatible(t *testing.T) {
	tests := []struct {
		name      string
		dir       string
		cachedDir string
		want      bool
	}{
		{"same dir", "/repo", "/repo", true},
		{"subdirectory", "/repo/sub", "/repo", true},
		{"other checkout", "/wt", "/repo", false},
		{"sibling sharing prefix", "/repo-copy", "/repo", false},
		{"empty query dir", "", "/repo", true},
		{"empty cached dir", "/repo", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dirCompatible(tt.dir, tt.cachedDir); got != tt.want {
				t.Errorf("dirCompatible(%q, %q) = %v, want %v", tt.dir, tt.cachedDir, got, tt.want)
			}
		})
	}
}

func TestGraphServer_LoadDiskCache_Retarget(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(t.TempDir(), "graph.json")
	saved := savedGraph{
		Resp:        []byte(`{"Packages":[{"GoFiles":["/old/checkout/pkg/a.go"]}]}`),
		PatternsKey: "./...",
		Patterns:    []string{"./..."},
		Dir:         "/old/checkout",
		Root:        "/old/checkout",
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, data, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &graphServer{log: log.New(io.Discard, "", 0), cacheFile: cache, root: root}
	g.loadDiskCache()

	g.mu.Lock()
	resp, dir := string(g.main.resp), g.main.dir
	g.mu.Unlock()
	wantResp := `{"Packages":[{"GoFiles":["` + root + `/pkg/a.go"]}]}`
	if resp != wantResp {
		t.Errorf("resp = %s, want %s", resp, wantResp)
	}
	if dir != root {
		t.Errorf("dir = %s, want %s", dir, root)
	}
}

func TestGraphServer_Answer_DirMismatch(t *testing.T) {
	otherDir := t.TempDir()
	g := &graphServer{
		log: log.New(io.Discard, "", 0),
		main: graphSlot{
			resp:        []byte(`{"Packages":[{"GoFiles":["/repo/pkg/a.go"]}]}`),
			patternsKey: "./...",
			patterns:    []string{"./..."},
			dir:         "/repo",
		},
	}

	// A query from another checkout must not be served the cached paths.
	got := g.answer(driverQuery{Patterns: []string{"./..."}, Dir: otherDir, Request: json.RawMessage(`{}`)})
	if !bytes.Equal(got, notHandled) {
		t.Errorf("answer(mismatched dir) = %s, want NotHandled", got)
	}
	g.mu.Lock()
	building := g.main.building
	g.mu.Unlock()
	if !building {
		t.Error("mismatched workspace query should trigger a rebuild for the new dir")
	}

	// A query from the cached dir itself is still served.
	g.mu.Lock()
	g.main.building = false
	g.mu.Unlock()
	got = g.answer(driverQuery{Patterns: []string{"./..."}, Dir: "/repo", Request: json.RawMessage(`{}`)})
	if bytes.Equal(got, notHandled) {
		t.Error("answer(matching dir) should serve the cache, got NotHandled")
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

// TestGraphServer_Answer_SubslotDispatch_NeverServedFromMainCache is the
// CRITICAL regression pin: a query whose Dir is under a nested module's
// go.mod must never be served from the main slot's cache, even though
// dirCompatible's pure string-prefix check would wrongly allow it here (the
// main slot's cached dir is the workspace root itself, and every
// subdirectory string-prefix-matches that). The modRoot dispatch at the very
// top of answer(), run BEFORE any hasCache/dirCompatible check, is what
// prevents this; this test would fail if that dispatch were removed or
// reordered after the dirCompatible check.
//
// A nested module's subslot answers with its own, synchronously built graph
// (answerNestedSlot) rather than NotHandled — see the doc comment on
// answerNestedSlot for why a cache miss cannot defer to gopls's own native go
// list fallback the way the main slot's can — so this pins "the response is
// not the main slot's cached bytes", not "the response is NotHandled".
func TestGraphServer_Answer_SubslotDispatch_NeverServedFromMainCache(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, "wt", "nested")
	writeGoMod(t, modDir)

	if !dirCompatible(modDir, root) {
		t.Fatal("test setup invalid: dirCompatible must consider modDir compatible with the main dir for this regression pin to be meaningful")
	}

	mainResp := []byte(`{"Packages":[{"GoFiles":["` + root + `/pkg/a.go"]}]}`)
	g := &graphServer{
		log:      log.New(io.Discard, "", 0),
		root:     root,
		modRoots: &moduleRootCache{},
		main: graphSlot{
			resp:        mainResp,
			patternsKey: "./...",
			patterns:    []string{"./..."},
			dir:         root,
		},
	}

	got := g.answer(driverQuery{Patterns: []string{"./..."}, Dir: modDir, Request: json.RawMessage(`{}`)})
	if bytes.Equal(got, mainResp) {
		t.Error("answer(nested module dir) returned the main slot's cached bytes, want the nested module's own graph")
	}
	if bytes.Equal(got, notHandled) {
		t.Error("answer(nested module dir) = NotHandled, want a synchronously built response for the nested module")
	}

	g.mu.Lock()
	_, gotSubslot := g.subslots[modDir]
	mainDirUnchanged := g.main.dir == root
	mainRespUnchanged := bytes.Equal(g.main.resp, mainResp)
	g.mu.Unlock()
	if !gotSubslot {
		t.Error("answer did not dispatch the nested-module query to its own subslot")
	}
	if !mainDirUnchanged || !mainRespUnchanged {
		t.Error("answer mutated the main slot while serving a nested-module query")
	}
}

// TestGraphServer_Answer_MainDirQuery_StillServedFromMainSlot verifies the
// dispatch's other half: a query whose Dir belongs to the main module (not
// under any nested module's go.mod) is unaffected by subslot dispatch and
// keeps being served from the main slot exactly as before.
func TestGraphServer_Answer_MainDirQuery_StillServedFromMainSlot(t *testing.T) {
	root := t.TempDir()
	writeGoMod(t, filepath.Join(root, "wt", "nested")) // a nested module exists elsewhere, but is irrelevant here

	g := &graphServer{
		log:      log.New(io.Discard, "", 0),
		root:     root,
		modRoots: &moduleRootCache{},
		main: graphSlot{
			resp:        []byte(`{"Packages":[{"GoFiles":["` + root + `/pkg/a.go"]}]}`),
			patternsKey: "./...",
			patterns:    []string{"./..."},
			dir:         root,
		},
	}

	got := g.answer(driverQuery{Patterns: []string{"./..."}, Dir: root, Request: json.RawMessage(`{}`)})
	if bytes.Equal(got, notHandled) {
		t.Error("answer(main root dir) should serve the main slot's cache, got NotHandled")
	}
	g.mu.Lock()
	n := len(g.subslots)
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("answer(main root dir) created %d subslots, want 0", n)
	}
}

// TestGraphServer_BuildSlot_SubslotSkipsDiskCache verifies that a subslot
// build (persist=false, the only way answer dispatches a nested module's
// build) never writes the on-disk cache — that cache's key is the git common
// dir, shared with the main slot, so a subslot write would corrupt it.
func TestGraphServer_BuildSlot_SubslotSkipsDiskCache(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir)
	mustWriteFile(t, filepath.Join(dir, "a.go"), "package x\n")

	cacheFile := filepath.Join(t.TempDir(), "graph.json")
	g := &graphServer{log: log.New(io.Discard, "", 0), cacheFile: cacheFile}

	slot := &graphSlot{}
	g.buildSlot(slot, []string{"./..."}, dir, "./...", nil, false)

	g.mu.Lock()
	resp := slot.resp
	g.mu.Unlock()
	if len(resp) == 0 {
		t.Fatal("buildSlot did not populate the subslot")
	}
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Errorf("buildSlot(persist=false) wrote a disk cache file at %s, want none", cacheFile)
	}
}

// TestGraphServer_MarkStaleFor_RoutesToOwningSlot verifies that
// MarkStaleFor marks stale only the slot owning path's module: a nested
// module's go.mod change must not touch the main slot's staleness, and vice
// versa.
func TestGraphServer_MarkStaleFor_RoutesToOwningSlot(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, "wt", "nested")
	writeGoMod(t, modDir)

	g := &graphServer{
		log:      log.New(io.Discard, "", 0),
		root:     root,
		modRoots: &moduleRootCache{},
		main:     graphSlot{patternsKey: "./...", patterns: []string{"./..."}, dir: root},
	}
	sub := &graphSlot{patternsKey: "./...", patterns: []string{"./..."}, dir: modDir}
	g.subslots = map[string]*graphSlot{modDir: sub}

	g.MarkStaleFor(filepath.Join(modDir, "go.mod"), "module file changed: go.mod")
	g.mu.Lock()
	subStale, mainStale := sub.stale, g.main.stale
	g.mu.Unlock()
	if !subStale {
		t.Error("MarkStaleFor(nested path) did not mark the owning subslot stale")
	}
	if mainStale {
		t.Error("MarkStaleFor(nested path) incorrectly marked the main slot stale")
	}

	g.MarkStaleFor(filepath.Join(root, "go.mod"), "module file changed: go.mod")
	g.mu.Lock()
	mainStale = g.main.stale
	g.mu.Unlock()
	if !mainStale {
		t.Error("MarkStaleFor(main-module path) did not mark the main slot stale")
	}
}

// TestGraphServer_MarkStaleFor_NoExistingSubslotIsNoop verifies that
// MarkStaleFor never lazily creates a subslot just to mark it stale: with
// nothing ever built for a module, there is no cache to invalidate.
func TestGraphServer_MarkStaleFor_NoExistingSubslotIsNoop(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, "wt", "nested")
	writeGoMod(t, modDir)

	g := &graphServer{
		log:      log.New(io.Discard, "", 0),
		root:     root,
		modRoots: &moduleRootCache{},
		main:     graphSlot{patternsKey: "./...", patterns: []string{"./..."}, dir: root},
	}

	g.MarkStaleFor(filepath.Join(modDir, "go.mod"), "module file changed: go.mod")

	g.mu.Lock()
	_, created := g.subslots[modDir]
	mainStale := g.main.stale
	g.mu.Unlock()
	if created {
		t.Error("MarkStaleFor lazily created a subslot with nothing to invalidate")
	}
	if mainStale {
		t.Error("MarkStaleFor(nested path, no existing subslot) incorrectly marked the main slot stale")
	}
}

// writeBuildTagFixture writes a tiny module with one file gated behind a
// non-default build tag alongside an always-built file in the same package,
// so a `go list` with vs. without -tags=sometag differs in whether
// tagged.go is part of the compiled package (see respHasCompiledFile).
func writeBuildTagFixture(t *testing.T, dir string) {
	t.Helper()
	writeGoMod(t, dir)
	mustWriteFile(t, filepath.Join(dir, "normal.go"), "package x\n\nfunc Normal() {}\n")
	mustWriteFile(t, filepath.Join(dir, "tagged.go"), "//go:build sometag\n\npackage x\n\nfunc Tagged() {}\n")
}

// respHasCompiledFile reports whether "tagged.go" (writeBuildTagFixture's
// gated file) appears in any package's CompiledGoFiles in a marshaled
// DriverResponse. Package.GoFiles deliberately includes files excluded by
// the current build tags (see its doc comment), so CompiledGoFiles is the
// field that actually reflects build-tag filtering.
func respHasCompiledFile(t *testing.T, resp []byte) bool {
	t.Helper()
	const name = "tagged.go"
	var r struct {
		Packages []struct {
			CompiledGoFiles []string
		}
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatalf("unmarshal DriverResponse: %v", err)
	}
	for _, p := range r.Packages {
		for _, f := range p.CompiledGoFiles {
			if filepath.Base(f) == name {
				return true
			}
		}
	}
	return false
}

func TestCanonicalKey(t *testing.T) {
	patterns := []string{"./..."}

	noFlags := canonicalKey(patterns, nil)
	if want := "./..."; noFlags != want {
		t.Errorf("canonicalKey(patterns, nil) = %q, want byte-identical to strings.Join(patterns, NUL) = %q", noFlags, want)
	}

	tagged := canonicalKey(patterns, []string{"-tags=sometag"})
	if tagged == noFlags {
		t.Error("canonicalKey with BuildFlags must differ from canonicalKey without BuildFlags")
	}

	other := canonicalKey(patterns, []string{"-tags=othertag"})
	if other == tagged {
		t.Error("canonicalKey with different BuildFlags must produce different keys")
	}
}

func TestNestedModuleCacheKey(t *testing.T) {
	noFlags := nestedModuleCacheKey(nil)
	if noFlags != nestedModuleCacheKeyBase {
		t.Errorf("nestedModuleCacheKey(nil) = %q, want %q", noFlags, nestedModuleCacheKeyBase)
	}
	tagged := nestedModuleCacheKey([]string{"-tags=sometag"})
	if tagged == noFlags {
		t.Error("nestedModuleCacheKey with BuildFlags must differ from the flags-less key")
	}
}

// TestGraphServer_BuildSlot_HonorsBuildFlags is the core regression pin for
// the field-reported bug: BuildFlags must actually reach `go list`.
func TestGraphServer_BuildSlot_HonorsBuildFlags(t *testing.T) {
	dir := t.TempDir()
	writeBuildTagFixture(t, dir)

	g := &graphServer{log: log.New(io.Discard, "", 0)}

	noTags := &graphSlot{}
	g.buildSlot(noTags, []string{"./..."}, dir, canonicalKey([]string{"./..."}, nil), nil, false)
	g.mu.Lock()
	respNoTags, keyNoTags := noTags.resp, noTags.patternsKey
	g.mu.Unlock()
	if respNoTags == nil {
		t.Fatal("buildSlot without BuildFlags failed to produce a response")
	}
	if respHasCompiledFile(t, respNoTags) {
		t.Error("response without -tags=sometag should not include the tagged file")
	}

	tagged := &graphSlot{}
	flags := []string{"-tags=sometag"}
	g.buildSlot(tagged, []string{"./..."}, dir, canonicalKey([]string{"./..."}, flags), flags, false)
	g.mu.Lock()
	respTagged, keyTagged, gotFlags := tagged.resp, tagged.patternsKey, tagged.buildFlags
	g.mu.Unlock()
	if respTagged == nil {
		t.Fatal("buildSlot with BuildFlags failed to produce a response")
	}
	if !respHasCompiledFile(t, respTagged) {
		t.Error("response with -tags=sometag should include the tagged file")
	}
	if keyNoTags == keyTagged {
		t.Error("slots built with different BuildFlags must have different patternsKey")
	}
	if len(gotFlags) != 1 || gotFlags[0] != "-tags=sometag" {
		t.Errorf("slot.buildFlags = %v, want %v", gotFlags, flags)
	}
}

// TestGraphServer_Answer_RegressionPin_TagLessCacheNotServedForTaggedQuery is
// the literal field-reported bug: a warm tag-less cache must never be served
// for a subsequently-arriving request with BuildFlags, and a request with
// BuildFlags must eventually be served the tagged graph once rebuilt.
func TestGraphServer_Answer_RegressionPin_TagLessCacheNotServedForTaggedQuery(t *testing.T) {
	dir := t.TempDir()
	writeBuildTagFixture(t, dir)

	g := &graphServer{log: log.New(io.Discard, "", 0), main: graphSlot{dir: dir}}

	// Warm a tag-less cache, as if built from an earlier query.
	g.buildSlot(&g.main, []string{"./..."}, dir, canonicalKey([]string{"./..."}, nil), nil, false)
	g.mu.Lock()
	tagLessResp := g.main.resp
	g.mu.Unlock()
	if respHasCompiledFile(t, tagLessResp) {
		t.Fatal("test setup invalid: warm cache unexpectedly includes the tagged file")
	}

	req, err := json.Marshal(packages.DriverRequest{BuildFlags: []string{"-tags=sometag"}})
	if err != nil {
		t.Fatal(err)
	}
	got := g.answer(driverQuery{Patterns: []string{dir + "/..."}, Dir: dir, Request: req})
	if bytes.Equal(got, tagLessResp) {
		t.Fatal("answer(tagged query) served the tag-less cache -- the literal bug this test pins")
	}

	waitFor(t, 15*time.Second, "background rebuild for the tagged query", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.main.building
	})

	got = g.answer(driverQuery{Patterns: []string{dir + "/..."}, Dir: dir, Request: req})
	if !respHasCompiledFile(t, got) {
		t.Error("answer(tagged query) after the triggered rebuild did not include the tagged file")
	}
}

// TestGraphServer_LoadDiskCache_RestoresBuildFlags verifies the disk-cache
// round trip: BuildFlags persisted to savedGraph are restored onto the slot,
// and a matching-flags query hits the restored cache while a
// different-flags query does not.
func TestGraphServer_LoadDiskCache_RestoresBuildFlags(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(t.TempDir(), "graph.json")
	flags := []string{"-tags=sometag"}
	patterns := []string{"./..."}
	key := canonicalKey(patterns, flags)
	saved := savedGraph{
		Resp:        []byte(`{"Packages":[{"GoFiles":["` + root + `/tagged.go"]}]}`),
		PatternsKey: key,
		Patterns:    patterns,
		BuildFlags:  flags,
		Dir:         root,
		Root:        root,
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, data, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &graphServer{log: log.New(io.Discard, "", 0), cacheFile: cache, root: root}
	g.loadDiskCache()

	g.mu.Lock()
	gotFlags, gotKey := g.main.buildFlags, g.main.patternsKey
	g.mu.Unlock()
	if len(gotFlags) != 1 || gotFlags[0] != "-tags=sometag" {
		t.Errorf("g.main.buildFlags = %v, want %v", gotFlags, flags)
	}
	if gotKey != key {
		t.Errorf("g.main.patternsKey = %q, want %q", gotKey, key)
	}

	sameReq, err := json.Marshal(packages.DriverRequest{BuildFlags: flags})
	if err != nil {
		t.Fatal(err)
	}
	got := g.answer(driverQuery{Patterns: patterns, Dir: root, Request: sameReq})
	if bytes.Equal(got, notHandled) {
		t.Error("answer(same BuildFlags) after disk-cache load = NotHandled, want the restored cache")
	}

	g.mu.Lock()
	g.main.building = false // undo any rebuild the same-flags query above may have triggered
	g.mu.Unlock()

	diffReq, err := json.Marshal(packages.DriverRequest{BuildFlags: []string{"-tags=other"}})
	if err != nil {
		t.Fatal(err)
	}
	got = g.answer(driverQuery{Patterns: patterns, Dir: root, Request: diffReq})
	if !bytes.Equal(got, notHandled) {
		t.Error("answer(different BuildFlags) after disk-cache load should not hit the restored cache")
	}
}

// TestGraphServer_MarkSlotStale_ReplaysBuildFlags verifies that a
// markSlotStale-triggered background rebuild reuses the BuildFlags of the
// graph it is refreshing, rather than silently dropping them.
func TestGraphServer_MarkSlotStale_ReplaysBuildFlags(t *testing.T) {
	dir := t.TempDir()
	writeBuildTagFixture(t, dir)

	g := &graphServer{log: log.New(io.Discard, "", 0)}
	patterns := []string{"./..."}
	flags := []string{"-tags=sometag"}
	key := canonicalKey(patterns, flags)
	g.buildSlot(&g.main, patterns, dir, key, flags, false)

	g.mu.Lock()
	initialResp := g.main.resp
	g.mu.Unlock()
	if !respHasCompiledFile(t, initialResp) {
		t.Fatal("test setup invalid: initial build did not honor BuildFlags")
	}
	g.mu.Lock()
	g.main.resp = nil // only pass below if markSlotStale actually rebuilt
	g.mu.Unlock()

	g.markSlotStale(&g.main, "test", false)

	waitFor(t, 15*time.Second, "markSlotStale timer firing", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.main.building
	})
	waitFor(t, 15*time.Second, "markSlotStale rebuild finishing", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.main.building
	})

	g.mu.Lock()
	resp, gotFlags := g.main.resp, g.main.buildFlags
	g.mu.Unlock()
	if !respHasCompiledFile(t, resp) {
		t.Error("markSlotStale rebuild dropped BuildFlags: tagged file missing from the rebuilt graph")
	}
	if len(gotFlags) != 1 || gotFlags[0] != "-tags=sometag" {
		t.Errorf("g.main.buildFlags after rebuild = %v, want %v", gotFlags, flags)
	}
}

// TestGraphServer_AnswerNestedSlot_DistinctBuildFlagsRebuild verifies that a
// nested subslot never collides two requests differing only in BuildFlags on
// the same cache entry: this is answerNestedSlot's synchronous-build path, so
// both queries below are answered inline with no need to wait.
func TestGraphServer_AnswerNestedSlot_DistinctBuildFlagsRebuild(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, "wt", "nested")
	writeBuildTagFixture(t, modDir)

	g := &graphServer{log: log.New(io.Discard, "", 0), root: root, modRoots: &moduleRootCache{}}

	noFlagsReq, err := json.Marshal(packages.DriverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := g.answer(driverQuery{Patterns: []string{modDir + "/..."}, Dir: modDir, Request: noFlagsReq})
	if respHasCompiledFile(t, got) {
		t.Fatal("nested subslot response without BuildFlags should not include the tagged file")
	}

	taggedReq, err := json.Marshal(packages.DriverRequest{BuildFlags: []string{"-tags=sometag"}})
	if err != nil {
		t.Fatal(err)
	}
	got = g.answer(driverQuery{Patterns: []string{modDir + "/..."}, Dir: modDir, Request: taggedReq})
	if !respHasCompiledFile(t, got) {
		t.Error("nested subslot response with BuildFlags did not include the tagged file (stale flags-less cache served, or key collision)")
	}
}
