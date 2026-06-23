package goplslazy

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseImports(t *testing.T) {
	src := []byte(`package x

import (
	"fmt"
	"example.com/mod/go/pkg/util"
	alias "example.com/mod/go/services/auth/internal"
	"example.com/other/thing"
)
`)
	all, internal := parseImports(src, "example.com/mod")
	wantAll := []string{"example.com/mod/go/pkg/util", "example.com/mod/go/services/auth/internal", "example.com/other/thing", "fmt"}
	wantInternal := []string{"go/pkg/util", "go/services/auth/internal"}
	if !reflect.DeepEqual(all, wantAll) {
		t.Errorf("all = %v, want %v", all, wantAll)
	}
	if !reflect.DeepEqual(internal, wantInternal) {
		t.Errorf("internal = %v, want %v", internal, wantInternal)
	}
}

func TestParseImports_EmbedSignature(t *testing.T) {
	without := []byte("package x\n\nimport \"embed\"\n\nvar fs embed.FS\n")
	with := []byte("package x\n\nimport \"embed\"\n\n//go:embed assets/*\nvar fs embed.FS\n")
	a1, _ := parseImports(without, "example.com/mod")
	a2, _ := parseImports(with, "example.com/mod")
	if reflect.DeepEqual(a1, a2) {
		t.Error("adding a //go:embed directive should change the file signature")
	}
	a3, _ := parseImports(with, "example.com/mod")
	if !reflect.DeepEqual(a2, a3) {
		t.Error("identical content should produce identical signatures")
	}
}

// buildTestIndex creates a small module on disk:
//
//	go/pkg/base          <- imported by go/pkg/mid and go/services/a
//	go/pkg/mid           <- imported by go/services/b (so b transitively imports base)
//	go/services/a, go/services/b, go/services/c (c imports nothing)
func buildTestIndex(t *testing.T) *revIndex {
	t.Helper()
	root := t.TempDir()
	write := func(rel, src string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/mod\n\ngo 1.26\n")
	write("go/pkg/base/base.go", "package base\n")
	write("go/pkg/mid/mid.go", "package mid\n\nimport _ \"example.com/mod/go/pkg/base\"\n")
	write("go/services/a/main.go", "package a\n\nimport _ \"example.com/mod/go/pkg/base\"\n")
	write("go/services/b/main.go", "package b\n\nimport _ \"example.com/mod/go/pkg/mid\"\n")
	write("go/services/c/main.go", "package c\n")

	ri := newRevIndex(log.New(io.Discard, "", 0))
	ri.Build(root)
	if !ri.Ready() {
		t.Fatal("index not ready after Build")
	}
	return ri
}

func TestClosureUnits(t *testing.T) {
	ri := buildTestIndex(t)

	got := ri.ClosureUnits("go/pkg/base", 3)
	want := []string{"go/pkg/base", "go/pkg/mid", "go/services/a", "go/services/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("closure(base) = %v, want %v", got, want)
	}

	got = ri.ClosureUnits("go/services/c", 3)
	want = []string{"go/services/c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("closure(c) = %v, want %v", got, want)
	}
}

func TestSameImports(t *testing.T) {
	ri := buildTestIndex(t)
	path := filepath.Join(ri.root, "go", "services", "b", "main.go")

	same := []byte("package b\n\nimport _ \"example.com/mod/go/pkg/mid\"\n\nfunc edited() {}\n")
	if !ri.SameImports(path, same) {
		t.Error("body-only edit should keep imports same")
	}
	diff := []byte("package b\n\nimport _ \"example.com/mod/go/pkg/base\"\n")
	if ri.SameImports(path, diff) {
		t.Error("changed import should be detected")
	}
	external := []byte("package b\n\nimport (\n\t_ \"example.com/mod/go/pkg/mid\"\n\t\"fmt\"\n)\n\nvar _ = fmt.Sprint\n")
	if ri.SameImports(path, external) {
		t.Error("added external import should be detected")
	}
}

func TestUpdateFileChangeDetection(t *testing.T) {
	ri := buildTestIndex(t)
	path := filepath.Join(ri.root, "go", "services", "c", "main.go")

	if changed := ri.UpdateFile(path); changed {
		t.Error("re-indexing an unchanged file should report no change")
	}
	if err := os.WriteFile(path, []byte("package c\n\nimport _ \"example.com/mod/go/pkg/base\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed := ri.UpdateFile(path); !changed {
		t.Error("adding an import should report a change")
	}
	got := ri.ClosureUnits("go/pkg/base", 3)
	found := false
	for _, u := range got {
		if u == "go/services/c" {
			found = true
		}
	}
	if !found {
		t.Errorf("closure(base) after update = %v, should include go/services/c", got)
	}
}
