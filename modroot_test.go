package goplslazy

import (
	"os"
	"path/filepath"
	"testing"
)

// writeGoMod creates an empty go.mod at dir/go.mod, creating dir first.
func writeGoMod(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("WriteFile go.mod: %v", err)
	}
}

func TestModuleRootCache_RootFor(t *testing.T) {
	t.Run("file directly under root with no go.mod anywhere", func(t *testing.T) {
		root := t.TempDir()
		writeGoMod(t, root)

		c := &moduleRootCache{}
		if got := c.RootFor(root, root); got != "" {
			t.Errorf("RootFor(root, root) = %q, want \"\"", got)
		}
	})

	t.Run("deep file with no nested go.mod anywhere", func(t *testing.T) {
		root := t.TempDir()
		writeGoMod(t, root)
		dir := filepath.Join(root, "a", "b", "c")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		c := &moduleRootCache{}
		if got := c.RootFor(dir, root); got != "" {
			t.Errorf("RootFor(%q, %q) = %q, want \"\"", dir, root, got)
		}
	})

	t.Run("nested module under a dot-dir path", func(t *testing.T) {
		root := t.TempDir()
		writeGoMod(t, root)
		modDir := filepath.Join(root, ".claude", "worktrees", "x")
		writeGoMod(t, modDir)
		file := filepath.Join(modDir, "sub", "file.go")
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		c := &moduleRootCache{}
		if got := c.RootFor(filepath.Dir(file), root); got != modDir {
			t.Errorf("RootFor = %q, want %q", got, modDir)
		}
	})

	t.Run("nested module under a plain visible dir path", func(t *testing.T) {
		root := t.TempDir()
		writeGoMod(t, root)
		modDir := filepath.Join(root, "wt", "foo")
		writeGoMod(t, modDir)
		file := filepath.Join(modDir, "sub", "file.go")
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		c := &moduleRootCache{}
		if got := c.RootFor(filepath.Dir(file), root); got != modDir {
			t.Errorf("RootFor = %q, want %q", got, modDir)
		}
	})

	t.Run("nested-nested modules resolve to the innermost go.mod", func(t *testing.T) {
		root := t.TempDir()
		writeGoMod(t, root)
		outer := filepath.Join(root, "outer")
		writeGoMod(t, outer)
		inner := filepath.Join(outer, "inner")
		writeGoMod(t, inner)
		file := filepath.Join(inner, "pkg", "file.go")
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		c := &moduleRootCache{}
		if got := c.RootFor(filepath.Dir(file), root); got != inner {
			t.Errorf("RootFor = %q, want innermost %q (not outer %q)", got, inner, outer)
		}
	})

	t.Run("cache invalidation picks up a go.mod created after the first call", func(t *testing.T) {
		root := t.TempDir()
		writeGoMod(t, root)
		modDir := filepath.Join(root, "wt", "bar")
		file := filepath.Join(modDir, "file.go")
		if err := os.MkdirAll(modDir, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		c := &moduleRootCache{}
		if got := c.RootFor(filepath.Dir(file), root); got != "" {
			t.Fatalf("RootFor before go.mod exists = %q, want \"\"", got)
		}

		// Create the go.mod after the first (now cached) call.
		writeGoMod(t, modDir)

		if got := c.RootFor(filepath.Dir(file), root); got != "" {
			t.Fatalf("RootFor after go.mod created but before Invalidate = %q, want stale cached \"\"", got)
		}

		c.Invalidate()

		if got := c.RootFor(filepath.Dir(file), root); got != modDir {
			t.Fatalf("RootFor after Invalidate = %q, want %q", got, modDir)
		}
	})
}
