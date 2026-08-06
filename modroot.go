package goplslazy

import (
	"os"
	"path/filepath"
	"sync"
)

// moduleRootCache detects nested Go modules — e.g. git worktrees with their
// own go.mod living anywhere under the workspace root — purely by walking
// upward from a file's directory looking for a go.mod. Detection is never
// path-shape-based (no hardcoded dot-dir or worktree naming): a module
// under ".claude/worktrees/x" and one under "wt/foo" are indistinguishable
// to it. Results are cached per directory since the walk runs on every
// didOpen.
type moduleRootCache struct {
	mu sync.Mutex
	// cache maps a directory to its nested module root; "" is a valid,
	// explicitly-cached result meaning "no nested module, use the workspace
	// root". The comma-ok form distinguishes that from "not cached yet".
	cache map[string]string
}

// RootFor returns the nearest directory at-or-above dir (bounded by root,
// inclusive) that contains its own go.mod. It returns "" when dir belongs to
// the workspace root's own module, i.e. no go.mod is found strictly between
// dir and root — root's own go.mod, if any, never counts as "nested".
func (c *moduleRootCache) RootFor(dir, root string) string {
	c.mu.Lock()
	if v, ok := c.cache[dir]; ok {
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()

	result := nestedModuleRoot(dir, root)

	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]string{}
	}
	c.cache[dir] = result
	c.mu.Unlock()

	return result
}

// Invalidate clears the whole cache. Called whenever any go.mod appears or
// disappears; a full clear is an acceptable trade-off over incremental
// invalidation since this is rare (module creation/deletion, not file edits).
func (c *moduleRootCache) Invalidate() {
	c.mu.Lock()
	c.cache = nil
	c.mu.Unlock()
}

// nestedModuleRoot walks upward from dir toward root looking for the nearest
// directory with its own go.mod, stopping at (and excluding) root itself.
func nestedModuleRoot(dir, root string) string {
	for dir != root {
		if hasGoMod(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without ever reaching the
			// workspace root; dir was not under root to begin with.
			return ""
		}
		dir = parent
	}
	return ""
}

// hasGoMod reports whether dir directly contains a go.mod file.
func hasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}
