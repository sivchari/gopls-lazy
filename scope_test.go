package goplslazy

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScopeUnit(t *testing.T) {
	tests := []struct {
		rel  string
		n    int
		want string
	}{
		{"go/services/auth/internal/tokens", 3, "go/services/auth"},
		{"go/services/auth", 3, "go/services/auth"},
		{"go/pkg", 3, "go/pkg"},
		{"pkg/kubelet/types", 2, "pkg/kubelet"},
		{"main", 3, "main"},
	}
	for _, tt := range tests {
		if got := scopeUnit(tt.rel, tt.n); got != tt.want {
			t.Errorf("scopeUnit(%q, %d) = %q, want %q", tt.rel, tt.n, got, tt.want)
		}
	}
}

func TestFiltersLocked_RootUnit(t *testing.T) {
	p := &proxy{scope: map[string]*scopeEntry{
		"go/services/auth": {open: map[string]bool{}},
	}}

	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{"-**", "+go/services/auth"}) {
		t.Errorf("scoped filters = %v", got)
	}

	p.scope["."] = &scopeEntry{open: map[string]bool{}}
	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{}) {
		t.Errorf("root-unit filters = %v, want no filters", got)
	}
}

// TestUnitFor_MainRootUnchanged pins today's behavior for a file directly in
// the workspace root: it must still map to the special unit "." even with a
// (nested-module-unaware, empty) moduleRootCache wired in.
func TestUnitFor_MainRootUnchanged(t *testing.T) {
	root := t.TempDir()
	p := &proxy{root: root, opts: options{granularity: 3}, modRoots: &moduleRootCache{}}

	unit, ok := p.unitFor(filepath.Join(root, "main.go"))
	if !ok || unit != "." {
		t.Fatalf("unitFor(root file) = (%q, %v), want (\".\", true)", unit, ok)
	}
}

// TestUnitFor_NestedModuleRootFile pins the critical constraint: a file
// living directly in a nested module's root directory must emit the
// module's root-relative path, never the bare "." sentinel (filtersLocked
// treats "." as "drop all filters entirely", which is wrong here).
func TestUnitFor_NestedModuleRootFile(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, ".claude", "worktrees", "x")
	writeGoMod(t, modDir)
	p := &proxy{root: root, opts: options{granularity: 3}, modRoots: &moduleRootCache{}}

	unit, ok := p.unitFor(filepath.Join(modDir, "main.go"))
	if !ok {
		t.Fatalf("unitFor: ok = false")
	}
	if unit == "." {
		t.Fatalf("unitFor = %q, must never be \".\" for a nested module root file", unit)
	}
	if want := ".claude/worktrees/x"; unit != want {
		t.Fatalf("unitFor = %q, want %q", unit, want)
	}
}

// TestUnitFor_NestedModuleDeepFile checks a file nested several directories
// deep inside a worktree module resolves to prefix + "/" + module-relative
// scope unit.
func TestUnitFor_NestedModuleDeepFile(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, "wt", "foo")
	writeGoMod(t, modDir)
	p := &proxy{root: root, opts: options{granularity: 3}, modRoots: &moduleRootCache{}}

	file := filepath.Join(modDir, "pkg", "sub", "file.go")
	unit, ok := p.unitFor(file)
	if !ok {
		t.Fatalf("unitFor: ok = false")
	}
	if want := "wt/foo/pkg/sub"; unit != want {
		t.Fatalf("unitFor = %q, want %q", unit, want)
	}
}

// TestUnitFor_GranularityTruncatesModuleRelativeOnly proves granularity
// truncation applies to the module-relative part, not to the full
// root-relative path (which would give a different, wrong answer).
func TestUnitFor_GranularityTruncatesModuleRelativeOnly(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, ".claude", "worktrees", "x") // 3 segments below root
	writeGoMod(t, modDir)
	p := &proxy{root: root, opts: options{granularity: 2}, modRoots: &moduleRootCache{}}

	file := filepath.Join(modDir, "pkg", "sub", "deep", "file.go") // module-relative dir: pkg/sub/deep
	unit, ok := p.unitFor(file)
	if !ok {
		t.Fatalf("unitFor: ok = false")
	}
	if want := ".claude/worktrees/x/pkg/sub"; unit != want {
		t.Fatalf("unitFor = %q, want %q", unit, want)
	}

	fullRelTruncated := scopeUnit(".claude/worktrees/x/pkg/sub/deep", 2)
	if unit == fullRelTruncated {
		t.Fatalf("unit %q must differ from full-root-path truncation %q", unit, fullRelTruncated)
	}
}

// TestUnitFor_DotDirWithoutGoModUsesMainModulePath proves nested-module
// detection is go.mod presence, not "starts with a dot": a dot-dir with no
// go.mod of its own stays on the outer/main-module path.
func TestUnitFor_DotDirWithoutGoModUsesMainModulePath(t *testing.T) {
	root := t.TempDir()
	p := &proxy{root: root, opts: options{granularity: 3}, modRoots: &moduleRootCache{}}

	file := filepath.Join(root, ".claude", "worktrees", "x", "file.go")
	unit, ok := p.unitFor(file)
	if !ok {
		t.Fatalf("unitFor: ok = false")
	}
	if want := scopeUnit(".claude/worktrees/x", 3); unit != want {
		t.Fatalf("unitFor = %q, want %q (main-module path, no go.mod present)", unit, want)
	}
}

// TestRestoreScope_DropsMissingDirs verifies restoreScope skips a saved unit
// whose directory no longer exists under root (e.g. a deleted worktree)
// while still restoring units whose directories exist.
func TestRestoreScope_DropsMissingDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkgexists"), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// "pkgmissing" is deliberately not created.

	cacheFile := scopeCacheFile(root)
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o750); err != nil {
		t.Fatalf("MkdirAll cache dir: %v", err)
	}
	data, err := json.Marshal(savedScope{Units: []string{".", "pkgexists", "pkgmissing"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, data, 0o600); err != nil {
		t.Fatalf("WriteFile cache: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cacheFile) })

	p := &proxy{scope: map[string]*scopeEntry{}, log: log.New(io.Discard, "", 0)}
	p.restoreScope(root)

	if _, ok := p.scope["."]; !ok {
		t.Errorf("expected unit \".\" to be restored")
	}
	if _, ok := p.scope["pkgexists"]; !ok {
		t.Errorf("expected unit \"pkgexists\" to be restored")
	}
	if _, ok := p.scope["pkgmissing"]; ok {
		t.Errorf("expected unit \"pkgmissing\" to be dropped (directory no longer exists)")
	}
}
