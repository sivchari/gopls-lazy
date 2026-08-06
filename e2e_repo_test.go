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

	svcFiles  []string // all svcCount services/svcNN/svc.go files
	svc00File string
	svc17File string
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
