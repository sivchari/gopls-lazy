package goplslazy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// svcCount is the number of generated services/svcNN packages. Every service
// imports lib/iface and lib/util, so a correct cross-package references
// answer must cover all of them.
const svcCount = 20

// repoLocs records the exact 0-based positions the E2E subtests query,
// captured while the synthetic monorepo is written so nothing is re-parsed
// at query time.
type repoLocs struct {
	utilFile string      // lib/util/util.go
	sumDecl  lspPosition // "Sum" in "func Sum(a, b int) int"

	svc05File string      // services/svc05/svc.go
	sumCall   lspPosition // "Sum" in the util.Sum call inside svc05
	getCall   lspPosition // "Get" in the s.Get call inside svc05

	ifaceFile    string      // lib/iface/iface.go
	storeGetDecl lspPosition // "Get" in the Store interface

	memFile    string      // lib/memstore/memstore.go
	memGetDecl lspPosition // "Get" in "func (m *Mem) Get(key string) string"

	svcFiles     []string // all svcCount services/svcNN/svc.go files
	svc00File    string
	svc17File    string
	svc17SumCall lspPosition // "Sum" in the util.Sum call inside svc17
}

// writeMonorepo writes a single-module synthetic monorepo into a temp dir:
//
//	lib/iface     Store interface
//	lib/memstore  Mem implements Store without importing lib/iface
//	lib/util      Sum, called by every service
//	services/svcNN (NN=00..19) import lib/iface + lib/util, never memstore
//	cmd/app       wires memstore.Mem into services/svc00.Handle
//
// The returned root is symlink-resolved because gopls reports resolved paths
// (macOS: /var -> /private/var) and location comparisons must match exactly.
func writeMonorepo(t *testing.T) (string, repoLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs repoLocs

	writeRepoFile(t, root, "go.mod", "module example.com/mono\n\ngo 1.21\n")

	const ifaceSrc = `package iface

// Store is the storage abstraction the services depend on.
type Store interface {
	Get(key string) string
}
`
	locs.ifaceFile = writeRepoFile(t, root, "lib/iface/iface.go", ifaceSrc)
	locs.storeGetDecl = mustPos(t, ifaceSrc, "Get(key string) string", "Get")

	const memSrc = `package memstore

// Mem is an in-memory store. It satisfies the iface.Store interface
// structurally, without importing lib/iface.
type Mem struct{}

// Get returns the stored value for key.
func (m *Mem) Get(key string) string {
	return "mem:" + key
}
`
	locs.memFile = writeRepoFile(t, root, "lib/memstore/memstore.go", memSrc)
	locs.memGetDecl = mustPos(t, memSrc, "func (m *Mem) Get", "Get")

	const utilSrc = `package util

// Sum adds two ints.
func Sum(a, b int) int {
	return a + b
}
`
	locs.utilFile = writeRepoFile(t, root, "lib/util/util.go", utilSrc)
	locs.sumDecl = mustPos(t, utilSrc, "func Sum", "Sum")

	for i := range svcCount {
		name := fmt.Sprintf("svc%02d", i)
		src := fmt.Sprintf(`package %s

import (
	"example.com/mono/lib/iface"
	"example.com/mono/lib/util"
)

// Handle serves one request through the store.
func Handle(s iface.Store) string {
	v := s.Get("k")
	if util.Sum(%d, 1) > 0 {
		return v
	}
	return ""
}
`, name, i)
		path := writeRepoFile(t, root, filepath.Join("services", name, "svc.go"), src)
		locs.svcFiles = append(locs.svcFiles, path)
		switch i {
		case 0:
			locs.svc00File = path
		case 5:
			locs.svc05File = path
			locs.sumCall = mustPos(t, src, "util.Sum(", "Sum")
			locs.getCall = mustPos(t, src, "s.Get(", "Get")
		case 17:
			locs.svc17File = path
			locs.svc17SumCall = mustPos(t, src, "util.Sum(", "Sum")
		}
	}

	const appSrc = `package main

import (
	"fmt"

	"example.com/mono/lib/memstore"
	"example.com/mono/services/svc00"
)

func main() {
	fmt.Println(svc00.Handle(&memstore.Mem{}))
}
`
	writeRepoFile(t, root, "cmd/app/main.go", appSrc)

	return root, locs
}

// nestedLocs records the exact 0-based positions the nested-worktree-module
// E2E subtests query, captured while the nested module is written so nothing
// is re-parsed at query time. See writeMonorepoWithNestedWorktree.
type nestedLocs struct {
	pkgAFile string      // nested module's pkg/a/a.go
	sumDecl  lspPosition // "Sum" in the nested module's own "func Sum(a, b int) int"

	pkgBFile    string      // nested module's pkg/b/b.go
	sumCall     lspPosition // "Sum" in the a.Sum(...) call inside pkg/b
	pkgBEndLine int         // 0-based index of pkg/b/b.go's last line, for a full-file range
}

// writeMonorepoWithNestedWorktree extends writeMonorepo with a second,
// independent Go module nested under a dot-directory of the same root — e.g.
// a git worktree checked out at ".claude/worktrees/wt1". The path shape
// (dot-dir) is illustrative only: nested-module detection (moduleRootCache,
// see modroot.go) walks upward from an opened file looking for the nearest
// go.mod and is entirely path-shape-agnostic. This exercises the
// historically suspicious dot-dir case without the code depending on it.
//
// The nested module needs no go.work file and no git worktree metadata to
// trigger gopls's zero-config view creation: a plain go.mod under the
// workspace folder is the whole trigger.
//
// pkg/a exports Sum, deliberately sharing its name with lib/util.Sum in the
// main fixture (repoLocs) so a references query rooted in the main module can
// be pinned to never surface the nested module's call site: the two Sums are
// different symbols in different packages/modules, so a correct answer
// naturally excludes the nested one even though the name collides
// textually.
//
// Since this fixture lives under a non-git t.TempDir(), graphCacheKey's
// git-common-dir resolution (see graph.go) falls through to its second
// fallback, the main module's own go.mod module path — expected and harmless
// here since none of these tests exercise the on-disk graph cache.
func writeMonorepoWithNestedWorktree(t *testing.T) (string, repoLocs, nestedLocs) {
	t.Helper()
	root, repo := writeMonorepo(t)

	var nested nestedLocs

	writeRepoFile(t, root, ".claude/worktrees/wt1/go.mod", "module example.com/wt1\n\ngo 1.21\n")

	const aSrc = `package a

// Sum adds two ints. Same name as example.com/mono/lib/util.Sum, on purpose:
// see writeMonorepoWithNestedWorktree.
func Sum(a, b int) int {
	return a + b
}
`
	nested.pkgAFile = writeRepoFile(t, root, ".claude/worktrees/wt1/pkg/a/a.go", aSrc)
	nested.sumDecl = mustPos(t, aSrc, "func Sum", "Sum")

	const bSrc = `package b

import "example.com/wt1/pkg/a"

// Call invokes the nested module's own Sum.
func Call() int {
	return a.Sum(1, 2)
}
`
	nested.pkgBFile = writeRepoFile(t, root, ".claude/worktrees/wt1/pkg/b/b.go", bSrc)
	nested.sumCall = mustPos(t, bSrc, "a.Sum(", "Sum")
	nested.pkgBEndLine = len(strings.Split(bSrc, "\n")) - 1

	return root, repo, nested
}

func writeRepoFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

// mustPos returns the 0-based LSP position of token on the first line of
// content containing lineSubstr. All generated sources are ASCII, so byte
// columns equal the UTF-16 columns LSP expects.
func mustPos(t *testing.T, content, lineSubstr, token string) lspPosition {
	t.Helper()
	for i, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, lineSubstr) {
			continue
		}
		col := strings.Index(line, token)
		if col < 0 {
			t.Fatalf("token %q not found on line %q", token, line)
		}
		return lspPosition{Line: i, Character: col}
	}
	t.Fatalf("no line contains %q", lineSubstr)
	return lspPosition{}
}
