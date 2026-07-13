package goplslazy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// filterExcludeAll is the directoryFilter pattern that excludes every package.
const filterExcludeAll = "-**"

// filtersLocked returns the directoryFilters for the current scope. The
// caller must hold p.mu.
func (p *proxy) filtersLocked() []string {
	dirs := make([]string, 0, len(p.scope))
	for d := range p.scope {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	// If any open file lives at the workspace root (unit "."), gopls has no
	// filter pattern that matches the root directory alone. Fall back to no
	// filters so gopls loads the full workspace. This is correct for small
	// repos (like gopls-lazy itself) and acceptable for large repos where
	// root-level Go files are uncommon.
	for _, d := range dirs {
		if d == "." {
			if len(p.userFilters) > 0 {
				return p.userFilters
			}
			return []string{}
		}
	}

	fs := []string{filterExcludeAll}
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
// workspace root.  Files that live directly in the workspace root are mapped
// to the special unit "." (the root).
func (p *proxy) unitFor(path string) (string, bool) {
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	if root == "" || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", false
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	if rel == "." {
		// File is directly in the workspace root (e.g. main.go in a small
		// single-package repo). Represent it as the root unit ".".
		return ".", true
	}
	return scopeUnit(filepath.ToSlash(rel), p.opts.granularity), true
}

// pushScope tells gopls that configuration changed; gopls then re-requests
// workspace/configuration, and the patched answer carries the new filters.
// It returns the new scope generation; callers that must not race the view
// swap wait for it with awaitApplied. The new scope is also persisted to
// disk so the next session can restore it.
func (p *proxy) pushScope() int {
	note := message{
		JSONRPC: jsonrpcVersion,
		Method:  "workspace/didChangeConfiguration",
		Params:  json.RawMessage(`{"settings":{}}`),
	}
	raw, err := json.Marshal(note)
	if err != nil {
		return 0 // unreachable for a static struct; 0 is an always-applied gen
	}
	p.mu.Lock()
	gen := p.stampScopeGenLocked()
	p.log.Printf("rescope: gen=%d filters=%v", gen, p.filtersLocked())
	p.mu.Unlock()
	p.toServer.write(raw)
	// Editors without workspace/configuration support never pull, so the
	// barrier would never advance; degrade to a timed advance instead.
	p.armAppliedFallback(gen)
	go p.saveScope()
	return gen
}

// ---- configuration-generation barrier ----------------------------------
//
// Scope changes reach gopls in three steps: pushScope sends
// workspace/didChangeConfiguration, gopls pulls workspace/configuration,
// and the proxy patches the editor's response with the new
// directoryFilters. gopls builds its new view only after receiving that
// patched response, and every gopls request that needs package metadata
// blocks internally on the view's metadata load (awaitLoaded). So "the
// patched response covering generation N has been written to gopls" is a
// reliable barrier: any request forwarded after that point sees at least
// the generation-N scope, with no log-scraping heuristics. appliedGen
// records the highest such generation; appliedCh broadcasts its advances.

// stampScopeGenLocked advances the scope generation and stamps every
// not-yet-published scope entry with it. Callers must hold p.mu.
func (p *proxy) stampScopeGenLocked() int {
	p.scopeGen++
	for _, e := range p.scope {
		if e.gen == 0 {
			e.gen = p.scopeGen
		}
	}
	return p.scopeGen
}

// ensureUnitsLocked makes sure every unit is present in the scope map and
// reports the generation barrier covering all of them: needPush is true when
// at least one unit is not yet published (the caller must run pushScope —
// without p.mu — and wait for the generation it returns); otherwise gen is
// the highest generation among the units, which is the one to await.
// Callers must hold p.mu.
func (p *proxy) ensureUnitsLocked(units []string) (gen int, needPush bool) {
	now := time.Now()
	for _, u := range units {
		e := p.scope[u]
		if e == nil {
			e = &scopeEntry{open: map[string]bool{}, lastActive: now}
			p.scope[u] = e
		}
		if e.gen == 0 {
			needPush = true
		} else if e.gen > gen {
			gen = e.gen
		}
	}
	return gen, needPush
}

// appliedChLocked returns the broadcast channel for the next appliedGen
// advance, allocating it on first use. Callers must hold p.mu.
func (p *proxy) appliedChLocked() chan struct{} {
	if p.appliedCh == nil {
		p.appliedCh = make(chan struct{})
	}
	return p.appliedCh
}

func (p *proxy) advanceApplied(gen int) {
	p.mu.Lock()
	p.advanceAppliedLocked(gen)
	p.mu.Unlock()
}

// advanceAppliedLocked moves the applied generation forward (never back)
// and wakes every awaitApplied/awaitUnitApplied waiter. Callers must hold
// p.mu.
func (p *proxy) advanceAppliedLocked(gen int) {
	if gen <= p.appliedGen {
		return
	}
	p.appliedGen = gen
	if p.appliedCh != nil {
		close(p.appliedCh)
		p.appliedCh = nil
	}
}

// awaitApplied blocks until gopls has received filters covering generation
// gen, or the timeout elapses. It reports whether the barrier was reached.
func (p *proxy) awaitApplied(gen int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		if p.appliedGen >= gen {
			p.mu.Unlock()
			return true
		}
		ch := p.appliedChLocked()
		p.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ch:
			timer.Stop()
		case <-timer.C:
			return false
		}
	}
}

// awaitUnitApplied blocks until the unit's scope entry has been published
// (gen stamped by a pushScope) AND that generation is applied. Publishing
// does not broadcast — only appliedGen advances do — so a coarse ticker
// covers the gen==0 -> stamped transition (e.g. a definition arriving inside
// the didOpen debounce window, before the rescope has fired).
func (p *proxy) awaitUnitApplied(unit string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		p.mu.Lock()
		e := p.scope[unit]
		done := e != nil && e.gen > 0 && e.gen <= p.appliedGen
		var ch chan struct{}
		if !done {
			ch = p.appliedChLocked()
		}
		p.mu.Unlock()
		if done {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ch:
		case <-tick.C:
		case <-timer.C:
			return false
		}
		timer.Stop()
	}
}

// armAppliedFallback advances the applied generation on a timer so a hold
// degrades to a bounded delay instead of hanging on an editor that never pulls
// workspace/configuration.
//
// Invariant: never advance appliedGen for a generation while gopls still has an
// unanswered config pull that will confirm it. An editor that DOES support
// pull configuration but is momentarily slow (busy UI, remote/SSH) may take
// longer than the timer to answer; advancing then would release the held
// request against the OLD narrow scope and silently truncate results. So once
// we have seen this editor pull configuration at all (sawConfigPull), the timer
// stops force-advancing and only re-arms — the real patched response advances
// appliedGen correctly. A truly dead config-supporting editor is still released
// after an absolute cap, with a warning. Editors that never pull keep the
// graceful timed advance.
func (p *proxy) armAppliedFallback(gen int) {
	d := p.appliedFallbackDelay
	if d == 0 {
		d = 10 * time.Second
	}
	capDur := p.appliedFallbackCap
	if capDur == 0 {
		capDur = 60 * time.Second
	}
	p.scheduleAppliedFallback(gen, d, time.Now().Add(capDur))
}

// scheduleAppliedFallback arms one fallback tick for gen, re-arming until the
// absolute deadline while a config-supporting editor is expected to answer.
func (p *proxy) scheduleAppliedFallback(gen int, d time.Duration, deadline time.Time) {
	time.AfterFunc(d, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.appliedGen >= gen {
			return // a config response already advanced past this generation
		}
		if p.sawConfigPull {
			// The editor answers configuration pulls, so the patched response
			// (which advances appliedGen correctly) is what should win. Wait for
			// it rather than releasing against the old scope.
			if remaining := time.Until(deadline); remaining > 0 {
				next := d
				if next > remaining {
					next = remaining
				}
				p.scheduleAppliedFallback(gen, next, deadline)
				return
			}
			p.log.Printf("rescope: config pull for gen %d still unanswered at cap; releasing anyway (results may be truncated)", gen)
			p.advanceAppliedLocked(gen)
			return
		}
		p.log.Printf("rescope: no workspace/configuration pull confirmed gen %d within %s; assuming applied", gen, d)
		p.advanceAppliedLocked(gen)
	})
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

// ---- scope persistence -------------------------------------------------------
//
// The active scope (set of directory units) is saved to disk after each
// rescope. On the next startup the proxy restores it as the initial
// directoryFilters so gopls starts loading those packages before the user
// even opens a file. This eliminates the "module '.' not in workspace" orphan
// error the user would otherwise see during the type-check warmup window.

type savedScope struct {
	Units []string `json:"units"`
}

// scopeCacheFile returns the path for the on-disk scope file, using the same
// root-based key as the graph cache so worktrees share state.
func scopeCacheFile(root string) string {
	cf := graphCacheFile(root)
	dir, base := filepath.Split(cf)
	// Replace "graph-<hash>.json" with "scope-<hash>.json".
	return filepath.Join(dir, "scope-"+strings.TrimPrefix(base, "graph-"))
}

// saveScope writes the current scope units to disk. Called in a goroutine
// from pushScope. Callers must NOT hold p.mu.
func (p *proxy) saveScope() {
	p.mu.Lock()
	root := p.root
	units := make([]string, 0, len(p.scope))
	for u := range p.scope {
		units = append(units, u)
	}
	p.mu.Unlock()
	if root == "" || len(units) == 0 {
		return
	}
	path := scopeCacheFile(root)
	b, err := json.Marshal(savedScope{Units: units})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// restoreScope loads the previously saved scope into p.scope so the initial
// directoryFilters include the services the user was editing last session.
// Must be called before patchInitialize sends the first filters to gopls.
// Callers must NOT hold p.mu.
func (p *proxy) restoreScope(root string) {
	path := scopeCacheFile(root)
	data, err := os.ReadFile(path) //nolint:gosec // path is the scope cache file constructed from the workspace root
	if err != nil {
		return // first run or no saved scope
	}
	var saved savedScope
	if err := json.Unmarshal(data, &saved); err != nil || len(saved.Units) == 0 {
		return
	}
	p.mu.Lock()
	for _, u := range saved.Units {
		if _, exists := p.scope[u]; !exists {
			p.scope[u] = &scopeEntry{open: map[string]bool{}, lastActive: time.Now()}
		}
	}
	p.mu.Unlock()
	p.log.Printf("scope restored from previous session: %v", saved.Units)
}
