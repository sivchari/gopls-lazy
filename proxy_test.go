package goplslazy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe io.Writer capturing frames written to gopls.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestProxy() (*proxy, *syncBuffer) {
	buf := &syncBuffer{}
	return &proxy{
		opts:        options{granularity: 3, debounce: time.Millisecond},
		scope:       map[string]*scopeEntry{},
		configIDs:   map[string]bool{},
		openDocs:    map[string]openDoc{},
		pendingDiag: map[string]bool{},
		pendingOwn:  map[string]chan *message{},
		toServer:    newFrameWriter(buf),
		toClient:    newFrameWriter(io.Discard),
		log:         log.New(io.Discard, "", 0),
		// Keep the per-push fallback timer inert unless a test opts in.
		appliedFallbackDelay: time.Hour,
	}, buf
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: condition not reached within %s", what, d)
}

func TestPushScope_StampsUnpublishedEntries(t *testing.T) {
	p, buf := newTestProxy()
	p.scope["a/b/c"] = &scopeEntry{open: map[string]bool{}}         // not yet published
	p.scope["x/y/z"] = &scopeEntry{open: map[string]bool{}, gen: 1} // published earlier
	p.scopeGen = 1

	gen := p.pushScope()

	if gen != 2 {
		t.Errorf("pushScope() = %d, want 2", gen)
	}
	if got := p.scope["a/b/c"].gen; got != 2 {
		t.Errorf("unpublished entry gen = %d, want 2 (stamped at publish)", got)
	}
	if got := p.scope["x/y/z"].gen; got != 1 {
		t.Errorf("published entry gen = %d, want 1 (first publish wins)", got)
	}
	if !strings.Contains(buf.String(), `"method":"workspace/didChangeConfiguration"`) {
		t.Errorf("pushScope did not send didChangeConfiguration; wrote: %q", buf.String())
	}
}

func TestAwaitApplied(t *testing.T) {
	t.Run("fast path when already applied", func(t *testing.T) {
		p, _ := newTestProxy()
		p.appliedGen = 3
		if !p.awaitApplied(2, 0) {
			t.Error("awaitApplied(2) = false with appliedGen=3, want true")
		}
	})
	t.Run("times out when never applied", func(t *testing.T) {
		p, _ := newTestProxy()
		if p.awaitApplied(5, 20*time.Millisecond) {
			t.Error("awaitApplied(5) = true with appliedGen=0, want timeout")
		}
	})
	t.Run("woken by advanceApplied", func(t *testing.T) {
		p, _ := newTestProxy()
		done := make(chan bool, 1)
		go func() { done <- p.awaitApplied(2, 5*time.Second) }()
		waitFor(t, time.Second, "waiter registered", func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.appliedCh != nil
		})
		p.advanceApplied(2)
		select {
		case ok := <-done:
			if !ok {
				t.Error("awaitApplied = false after advanceApplied(2), want true")
			}
		case <-time.After(time.Second):
			t.Error("awaitApplied not woken by advanceApplied")
		}
	})
	t.Run("advance never moves backwards", func(t *testing.T) {
		p, _ := newTestProxy()
		p.advanceApplied(4)
		p.advanceApplied(2)
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.appliedGen != 4 {
			t.Errorf("appliedGen = %d after advance(4) then advance(2), want 4", p.appliedGen)
		}
	})
}

func TestHandleClientResponse(t *testing.T) {
	tests := []struct {
		name        string
		id          string // raw JSON id; empty means no id
		isConfig    bool
		gen         int
		wantHandled bool
		wantApplied int
	}{
		{name: "config response advances applied gen", id: "7", isConfig: true, gen: 3, wantHandled: true, wantApplied: 3},
		{name: "non-config response passes through", id: "8", wantHandled: false, wantApplied: 0},
		{name: "response without id passes through", wantHandled: false, wantApplied: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, buf := newTestProxy()
			if tt.isConfig {
				p.configIDs[tt.id] = true
				p.configGens = map[string]int{tt.id: tt.gen}
			}
			m := message{JSONRPC: jsonrpcVersion, Result: json.RawMessage(`[{}]`)}
			if tt.id != "" {
				m.ID = json.RawMessage(tt.id)
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}

			handled := p.handleClientResponse(raw, &m)

			if handled != tt.wantHandled {
				t.Errorf("handleClientResponse() = %v, want %v", handled, tt.wantHandled)
			}
			p.mu.Lock()
			applied := p.appliedGen
			pendingID := p.configIDs[tt.id]
			p.mu.Unlock()
			if applied != tt.wantApplied {
				t.Errorf("appliedGen = %d, want %d", applied, tt.wantApplied)
			}
			if tt.wantHandled {
				// The patched response must already be on the wire when
				// handleClientResponse returns (write happens before the
				// generation advance).
				if !strings.Contains(buf.String(), "directoryFilters") {
					t.Errorf("patched config response not written to gopls; wrote: %q", buf.String())
				}
				if pendingID {
					t.Error("config id not cleared after response")
				}
			} else if buf.String() != "" {
				t.Errorf("non-config response must not be written by the handler; wrote: %q", buf.String())
			}
		})
	}
}

func TestPushScope_AppliedFallbackWithoutConfigPull(t *testing.T) {
	p, _ := newTestProxy()
	p.appliedFallbackDelay = 10 * time.Millisecond
	p.scope["a/b/c"] = &scopeEntry{open: map[string]bool{}}

	gen := p.pushScope()

	if !p.awaitApplied(gen, 2*time.Second) {
		t.Fatalf("gen %d never applied: fallback timer did not fire", gen)
	}
}

func TestArmAppliedFallback_WaitsForSlowConfigPull(t *testing.T) {
	// A config-supporting editor that is merely slow must not have the timer
	// advance appliedGen before its real config response arrives; the fallback
	// re-arms instead so the patched response (which advances correctly) wins.
	p, _ := newTestProxy()
	p.appliedFallbackDelay = 10 * time.Millisecond
	p.appliedFallbackCap = time.Hour // do not hit the cap during this test
	p.sawConfigPull = true           // editor is known to answer config pulls
	p.scope["a/b/c"] = &scopeEntry{open: map[string]bool{}}

	gen := p.pushScope()

	// Give the timer many ticks; it must keep re-arming, not advance.
	time.Sleep(80 * time.Millisecond)
	p.mu.Lock()
	applied := p.appliedGen
	p.mu.Unlock()
	if applied >= gen {
		t.Fatalf("fallback advanced appliedGen to %d for gen %d while a config pull was outstanding", applied, gen)
	}
	// The real patched response advances it; the next tick then stops re-arming.
	p.advanceApplied(gen)
	if !p.awaitApplied(gen, time.Second) {
		t.Fatalf("gen %d not applied after the config response", gen)
	}
}

func TestArmAppliedFallback_ReleasesAtCapForDeadConfigEditor(t *testing.T) {
	// A config-supporting editor that never answers must still release the hold
	// once the absolute cap elapses, so a dead editor does not hang forever.
	p, _ := newTestProxy()
	p.appliedFallbackDelay = 5 * time.Millisecond
	p.appliedFallbackCap = 30 * time.Millisecond
	p.sawConfigPull = true
	p.scope["a/b/c"] = &scopeEntry{open: map[string]bool{}}

	gen := p.pushScope()

	if !p.awaitApplied(gen, time.Second) {
		t.Fatalf("gen %d never released; the cap did not fire for a dead config-supporting editor", gen)
	}
}

func TestIsCrossRef(t *testing.T) {
	tests := []struct {
		method string
		want   bool
	}{
		{methodRename, true},
		{methodReferences, true},
		{methodImplementation, true},
		{methodPrepareRename, false}, // light single-unit path, not the closure
		{methodDefinition, false},
	}
	for _, tt := range tests {
		if got := isCrossRef(tt.method); got != tt.want {
			t.Errorf("isCrossRef(%q) = %v, want %v", tt.method, got, tt.want)
		}
	}
}

func TestPrepareRenameTakesLightPath(t *testing.T) {
	// prepareRename must route to the light single-unit path: with the
	// requesting unit already applied it forwards inline (interceptClientRequest
	// returns false), whereas the cross-ref path always takes over an in-root
	// request (returns true) and pays the closure/worker cost.
	const uri = "file:///ws/go/services/auth/handler.go"
	const unit = "go/services/auth"
	p, buf := newTestProxy()
	p.root = "/ws"
	p.appliedGen = 1
	p.scope[unit] = &scopeEntry{open: map[string]bool{}, gen: 1} // applied

	raw := []byte(`{"jsonrpc":"2.0","id":9,"method":"textDocument/prepareRename",` +
		`"params":{"textDocument":{"uri":"` + uri + `"},"position":{"line":1,"character":2}}}`)
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	if p.interceptClientRequest(raw, &m) {
		t.Fatal("prepareRename with an applied unit was taken over; want inline forward (light path)")
	}
	if buf.String() != "" {
		t.Errorf("light path must not write anything for an applied unit; wrote %q", buf.String())
	}
}

func TestInterceptPrepareRename_HoldsUntilUnitApplied(t *testing.T) {
	const uri = "file:///ws/go/services/auth/handler.go"
	const unit = "go/services/auth"
	p, buf := newTestProxy()
	p.root = "/ws"
	p.appliedGen = 1
	p.scope[unit] = &scopeEntry{open: map[string]bool{}, gen: 2} // published, not applied

	raw := []byte(`{"jsonrpc":"2.0","id":9,"method":"textDocument/prepareRename",` +
		`"params":{"textDocument":{"uri":"` + uri + `"},"position":{"line":1,"character":2}}}`)
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	if !p.interceptPrepareRename(raw, &m) {
		t.Fatal("prepareRename for a pending unit should be held")
	}
	if strings.Contains(buf.String(), "prepareRename") {
		t.Fatal("held prepareRename forwarded before the unit was applied")
	}
	p.advanceApplied(2)
	waitFor(t, 2*time.Second, "held prepareRename released", func() bool {
		return strings.Contains(buf.String(), "textDocument/prepareRename")
	})
}

func TestReleaseHeldDefsOnDidChange(t *testing.T) {
	// A held definition carries the pre-edit cursor position; a didChange for
	// the same document must release it immediately (before the edit reaches
	// gopls), and the two possible forwarders must write it exactly once.
	const uri = "file:///ws/go/services/auth/handler.go"
	const unit = "go/services/auth"
	p, buf := newTestProxy()
	p.root = "/ws"
	p.appliedGen = 1
	p.scope[unit] = &scopeEntry{open: map[string]bool{}, gen: 2} // pending

	raw := []byte(`{"jsonrpc":"2.0","id":9,"method":"textDocument/definition",` +
		`"params":{"textDocument":{"uri":"` + uri + `"},"position":{"line":1,"character":2}}}`)
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	if !p.interceptDefinition(raw, &m) {
		t.Fatal("definition for a pending unit should be held")
	}
	if strings.Contains(buf.String(), "definition") {
		t.Fatal("held definition forwarded before release")
	}

	// didChange path releases it without the unit ever becoming applied.
	p.releaseHeldDefs(uri)
	// Unblock the waiter goroutine so its (idempotent) forward also runs.
	p.advanceApplied(2)
	waitFor(t, time.Second, "held definition released on didChange", func() bool {
		return strings.Contains(buf.String(), "textDocument/definition")
	})
	time.Sleep(50 * time.Millisecond) // give the waiter a chance to double-write
	if n := strings.Count(buf.String(), "textDocument/definition"); n != 1 {
		t.Errorf("held definition written %d times, want exactly 1 (sync.Once)", n)
	}
}

func TestHandleWorkerError(t *testing.T) {
	// prepareRename no longer reaches the worker, so handleWorkerError must not
	// special-case it (the removed dead case returned a retryable error). Only
	// rename fails loudly; references/implementation/prepareRename forward.
	tests := []struct {
		name        string
		method      string
		wantForward bool // true: held forwarded to gopls; false: retryable error to client
	}{
		{"rename returns retryable error", methodRename, false},
		{"prepareRename falls through (dead case removed)", methodPrepareRename, true},
		{"references forwards to main gopls", methodReferences, true},
		{"implementation forwards to main gopls", methodImplementation, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, buf := newTestProxy()
			held := []byte(`{"jsonrpc":"2.0","id":5,"method":"` + tt.method + `"}`)
			p.handleWorkerError(tt.method, json.RawMessage(`5`), held, fmt.Errorf("boom"))
			forwarded := strings.Contains(buf.String(), tt.method)
			if forwarded != tt.wantForward {
				t.Errorf("forwarded=%v want %v (wrote %q)", forwarded, tt.wantForward, buf.String())
			}
		})
	}
}

func TestInterceptDefinition(t *testing.T) {
	const uri = "file:///ws/go/services/auth/handler.go"
	const unit = "go/services/auth"
	tests := []struct {
		name       string
		uri        string
		entry      *scopeEntry // nil = unit absent from scope map
		appliedGen int
		wantHold   bool
	}{
		{name: "file outside root forwards", uri: "file:///elsewhere/x.go", wantHold: false},
		{name: "unit absent forwards (peek on never-opened file)", uri: uri, entry: nil, wantHold: false},
		{name: "unit applied forwards inline", uri: uri, entry: &scopeEntry{gen: 1}, appliedGen: 1, wantHold: false},
		{name: "unit published but not applied holds", uri: uri, entry: &scopeEntry{gen: 2}, appliedGen: 1, wantHold: true},
		{name: "unit unpublished holds", uri: uri, entry: &scopeEntry{gen: 0}, appliedGen: 1, wantHold: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, buf := newTestProxy()
			p.root = "/ws"
			p.appliedGen = tt.appliedGen
			if tt.entry != nil {
				tt.entry.open = map[string]bool{}
				p.scope[unit] = tt.entry
			}
			raw := []byte(`{"jsonrpc":"2.0","id":9,"method":"textDocument/definition",` +
				`"params":{"textDocument":{"uri":"` + tt.uri + `"},"position":{"line":1,"character":2}}}`)
			var m message
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}

			got := p.interceptDefinition(raw, &m)

			if got != tt.wantHold {
				t.Fatalf("interceptDefinition() = %v, want %v", got, tt.wantHold)
			}
			if !tt.wantHold {
				return
			}
			// Nothing may be forwarded while the unit is pending.
			if strings.Contains(buf.String(), "textDocument/definition") {
				t.Fatal("held definition forwarded before the unit was applied")
			}
			// Applying the unit's generation must release the held request.
			p.mu.Lock()
			p.scope[unit].gen = 2 // no-op unless the entry was unpublished
			p.advanceAppliedLocked(2)
			p.mu.Unlock()
			waitFor(t, 2*time.Second, "held definition released", func() bool {
				return strings.Contains(buf.String(), "textDocument/definition")
			})
		})
	}
}

// TestInterceptInlayHint mirrors TestInterceptDefinition: inlayHint (and
// hover, routed the same way) must hold while the requesting file's unit is
// pending and forward inline once it is already applied. Unlike definition,
// unforwardable/peek cases are not exercised here since inlayHint has no
// "peek on a never-opened file" use case in editors.
func TestInterceptInlayHint(t *testing.T) {
	const uri = "file:///ws/go/services/auth/handler.go"
	const unit = "go/services/auth"
	tests := []struct {
		name       string
		entry      *scopeEntry
		appliedGen int
		wantHold   bool
	}{
		{name: "unit applied forwards inline", entry: &scopeEntry{gen: 1}, appliedGen: 1, wantHold: false},
		{name: "unit published but not applied holds", entry: &scopeEntry{gen: 2}, appliedGen: 1, wantHold: true},
		{name: "unit unpublished holds", entry: &scopeEntry{gen: 0}, appliedGen: 1, wantHold: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, buf := newTestProxy()
			p.root = "/ws"
			p.appliedGen = tt.appliedGen
			tt.entry.open = map[string]bool{}
			p.scope[unit] = tt.entry
			raw := []byte(`{"jsonrpc":"2.0","id":9,"method":"textDocument/inlayHint",` +
				`"params":{"textDocument":{"uri":"` + uri + `"},"range":{"start":{"line":0,"character":0},"end":{"line":10,"character":0}}}}`)
			var m message
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}

			got := p.interceptClientRequest(raw, &m)

			if got != tt.wantHold {
				t.Fatalf("interceptClientRequest() = %v, want %v", got, tt.wantHold)
			}
			if !tt.wantHold {
				if buf.String() != "" {
					t.Errorf("unheld inlayHint must be forwarded inline by pumpClient, not by interceptClientRequest; wrote %q", buf.String())
				}
				return
			}
			// Nothing may be forwarded while the unit is pending.
			if strings.Contains(buf.String(), "textDocument/inlayHint") {
				t.Fatal("held inlayHint forwarded before the unit was applied")
			}
			// Applying the unit's generation must release the held request.
			p.mu.Lock()
			p.scope[unit].gen = 2 // no-op unless the entry was unpublished
			p.advanceAppliedLocked(2)
			p.mu.Unlock()
			waitFor(t, 2*time.Second, "held inlayHint released", func() bool {
				return strings.Contains(buf.String(), "textDocument/inlayHint")
			})
		})
	}
}

// TestReleaseHeldInlayHintOnDidChange mirrors TestReleaseHeldDefsOnDidChange:
// a held inlayHint must be released by a didChange for the same document,
// without waiting for the unit to be applied, and written exactly once.
func TestReleaseHeldInlayHintOnDidChange(t *testing.T) {
	const uri = "file:///ws/go/services/auth/handler.go"
	const unit = "go/services/auth"
	p, buf := newTestProxy()
	p.root = "/ws"
	p.appliedGen = 1
	p.scope[unit] = &scopeEntry{open: map[string]bool{}, gen: 2} // pending

	raw := []byte(`{"jsonrpc":"2.0","id":9,"method":"textDocument/inlayHint",` +
		`"params":{"textDocument":{"uri":"` + uri + `"},"range":{"start":{"line":0,"character":0},"end":{"line":10,"character":0}}}}`)
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	if !p.interceptClientRequest(raw, &m) {
		t.Fatal("inlayHint for a pending unit should be held")
	}
	if strings.Contains(buf.String(), "inlayHint") {
		t.Fatal("held inlayHint forwarded before release")
	}

	// didChange path releases it without the unit ever becoming applied.
	p.releaseHeldDefs(uri)
	// Unblock the waiter goroutine so its (idempotent) forward also runs.
	p.advanceApplied(2)
	waitFor(t, time.Second, "held inlayHint released on didChange", func() bool {
		return strings.Contains(buf.String(), "textDocument/inlayHint")
	})
	time.Sleep(50 * time.Millisecond) // give the waiter a chance to double-write
	if n := strings.Count(buf.String(), "textDocument/inlayHint"); n != 1 {
		t.Errorf("held inlayHint written %d times, want exactly 1 (sync.Once)", n)
	}
}

func TestEnsureUnitsLocked(t *testing.T) {
	tests := []struct {
		name     string
		scope    map[string]int // unit -> gen (entries created with that gen)
		units    []string
		wantGen  int
		wantPush bool
	}{
		{name: "no units", scope: map[string]int{}, units: nil, wantGen: 0, wantPush: false},
		{name: "all absent", scope: map[string]int{}, units: []string{"a", "b"}, wantGen: 0, wantPush: true},
		{name: "present but unpublished", scope: map[string]int{"a": 0}, units: []string{"a"}, wantGen: 0, wantPush: true},
		{name: "all published", scope: map[string]int{"a": 2, "b": 5}, units: []string{"a", "b"}, wantGen: 5, wantPush: false},
		{name: "mixed published and absent", scope: map[string]int{"a": 3}, units: []string{"a", "b"}, wantGen: 3, wantPush: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := newTestProxy()
			for u, gen := range tt.scope {
				p.scope[u] = &scopeEntry{open: map[string]bool{}, gen: gen}
			}

			p.mu.Lock()
			gen, needPush := p.ensureUnitsLocked(tt.units)
			p.mu.Unlock()

			if gen != tt.wantGen || needPush != tt.wantPush {
				t.Errorf("ensureUnitsLocked(%v) = (%d, %v), want (%d, %v)",
					tt.units, gen, needPush, tt.wantGen, tt.wantPush)
			}
			for _, u := range tt.units {
				if p.scope[u] == nil {
					t.Errorf("unit %s not added to scope", u)
				}
			}
		})
	}
}

func TestAwaitUnitApplied_TickerSeesSilentTransition(t *testing.T) {
	// A unit stamped and applied without an appliedCh broadcast (e.g. the
	// test mutates state directly) must still be noticed by the ticker.
	p, _ := newTestProxy()
	p.scope["u/v/w"] = &scopeEntry{open: map[string]bool{}}
	done := make(chan bool, 1)
	go func() { done <- p.awaitUnitApplied("u/v/w", 5*time.Second) }()

	time.Sleep(20 * time.Millisecond)
	p.mu.Lock()
	p.scope["u/v/w"].gen = 1
	p.appliedGen = 1 // deliberately bypasses advanceAppliedLocked's broadcast
	p.mu.Unlock()

	select {
	case ok := <-done:
		if !ok {
			t.Error("awaitUnitApplied = false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Error("awaitUnitApplied did not observe the stamped+applied unit")
	}
}

func TestInitialStampMarksRestoredScopeApplied(t *testing.T) {
	// patchInitialize publishes restored scope entries via the initialize
	// request itself: stamp + advance must leave them immediately usable.
	p, _ := newTestProxy()
	p.scope["go/services/auth"] = &scopeEntry{open: map[string]bool{}} // restored

	p.mu.Lock()
	p.advanceAppliedLocked(p.stampScopeGenLocked())
	stamped := p.scope["go/services/auth"].gen
	applied := p.appliedGen
	p.mu.Unlock()

	if stamped != 1 || applied != 1 {
		t.Errorf("gen = %d, appliedGen = %d after initial stamp, want 1, 1", stamped, applied)
	}
	if !p.awaitUnitApplied("go/services/auth", 0) {
		t.Error("restored unit not immediately applied after initialize stamping")
	}
}

func TestStripUserFilters(t *testing.T) {
	settings := map[string]any{
		"directoryFilters":       []any{"-**", "+a"},
		"build.directoryFilters": []any{"-**", "+b"},
		"build": map[string]any{
			"directoryFilters": []any{"-**", "+c"},
			"env":              map[string]any{"GOFLAGS": "-tags=x"},
		},
		"ui.semanticTokens": true,
	}
	stripUserFilters(settings)

	for _, k := range []string{"directoryFilters", "build.directoryFilters"} {
		if _, ok := settings[k]; ok {
			t.Errorf("%s should be removed", k)
		}
	}
	build, _ := settings["build"].(map[string]any)
	if _, ok := build["directoryFilters"]; ok {
		t.Error("build.directoryFilters (nested) should be removed")
	}
	if _, ok := build["env"]; !ok {
		t.Error("other nested build settings should be kept")
	}
	if v, _ := settings["ui.semanticTokens"].(bool); !v {
		t.Error("unrelated settings should be kept")
	}
}

// The editor's own directoryFilters (in any spelling gopls accepts) must not
// reach gopls next to the proxy's injected value: gopls uses only the last
// segment of a dotted name, so "build.directoryFilters" is the same option and
// a duplicate makes the applied value depend on map iteration order.
func TestPatchInitialize_StripsUserFilterVariants(t *testing.T) {
	p := &proxy{scope: map[string]*scopeEntry{}, log: log.New(io.Discard, "", 0)}
	params, err := json.Marshal(map[string]any{
		"initializationOptions": map[string]any{
			"build.directoryFilters": []any{"-**", "+go/services/hr"},
			"ui.semanticTokens":      true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &message{JSONRPC: jsonrpcVersion, ID: json.RawMessage("1"), Method: "initialize", Params: params}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	out := p.patchInitialize(raw, m)

	var patched struct {
		Params struct {
			InitializationOptions map[string]any `json:"initializationOptions"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &patched); err != nil {
		t.Fatal(err)
	}
	opts := patched.Params.InitializationOptions
	if _, ok := opts["build.directoryFilters"]; ok {
		t.Error("user's build.directoryFilters should be removed from initialize")
	}
	if got, want := opts["directoryFilters"], []any{"-**"}; !reflect.DeepEqual(got, want) {
		t.Errorf("directoryFilters = %v, want %v (proxy-managed only)", got, want)
	}
	if v, _ := opts["ui.semanticTokens"].(bool); !v {
		t.Error("unrelated initialization options should be kept")
	}

	// The isolated worker must see no directoryFilters at all, so its scope is
	// never truncated by the user's allowlist.
	var workerParams struct {
		InitializationOptions map[string]any `json:"initializationOptions"`
	}
	if err := json.Unmarshal(p.workerInit, &workerParams); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"directoryFilters", "build.directoryFilters"} {
		if _, ok := workerParams.InitializationOptions[k]; ok {
			t.Errorf("worker init params should not contain %s", k)
		}
	}
}

func TestPatchConfigResponse_StripsUserFilterVariants(t *testing.T) {
	p := &proxy{
		scope: map[string]*scopeEntry{"go/services/hr": {open: map[string]bool{}}},
		log:   log.New(io.Discard, "", 0),
	}
	result, err := json.Marshal([]any{map[string]any{
		"build.directoryFilters":    []any{"-**", "+go/services/hr"},
		"ui.diagnostic.staticcheck": false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := &message{JSONRPC: jsonrpcVersion, ID: json.RawMessage("1"), Result: result}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	out := p.patchConfigResponse(raw, m)

	var patched struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(out, &patched); err != nil {
		t.Fatal(err)
	}
	if len(patched.Result) != 1 {
		t.Fatalf("result items = %d, want 1", len(patched.Result))
	}
	item := patched.Result[0]
	if _, ok := item["build.directoryFilters"]; ok {
		t.Error("user's build.directoryFilters should be removed from configuration response")
	}
	if got, want := item["directoryFilters"], []any{"-**", "+go/services/hr"}; !reflect.DeepEqual(got, want) {
		t.Errorf("directoryFilters = %v, want %v (proxy-managed only)", got, want)
	}
	if _, ok := item["ui.diagnostic.staticcheck"]; !ok {
		t.Error("unrelated configuration settings should be kept")
	}

	var workerItem map[string]any
	if err := json.Unmarshal(p.workerConfig[0], &workerItem); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"directoryFilters", "build.directoryFilters"} {
		if _, ok := workerItem[k]; ok {
			t.Errorf("worker config should not contain %s", k)
		}
	}
}

func TestIndexFor_EmptyModRootReturnsMainIndex(t *testing.T) {
	p, _ := newTestProxy()
	p.idx = newRevIndex(log.New(io.Discard, "", 0))

	if got := p.indexFor(""); got != p.idx {
		t.Fatalf("indexFor(\"\") = %p, want p.idx %p", got, p.idx)
	}
}

// TestIndexFor_LazyBuildsAndDedupsNestedModule verifies indexFor creates a
// dedicated sub-index for a never-seen module root, builds it in the
// background, and returns the SAME instance on a second call (no duplicate
// build).
func TestIndexFor_LazyBuildsAndDedupsNestedModule(t *testing.T) {
	p, _ := newTestProxy()
	p.idx = newRevIndex(log.New(io.Discard, "", 0))

	modRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(modRoot, "go.mod"), []byte("module example.com/nested\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modRoot, "x.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := p.indexFor(modRoot)
	if first == p.idx {
		t.Fatal("indexFor(modRoot) returned the main index, want a dedicated sub-index")
	}
	if !first.WaitReady(2 * time.Second) {
		t.Fatal("sub-index did not become ready")
	}
	if first.root != modRoot {
		t.Fatalf("sub-index root = %q, want %q", first.root, modRoot)
	}

	second := p.indexFor(modRoot)
	if second != first {
		t.Fatal("indexFor(modRoot) called twice returned different instances, want the same cached instance (no duplicate build)")
	}
}

// TestInterceptWorkspaceSymbol_MergesSubIndexes proves interceptWorkspaceSymbol
// queries the main index and every ready sub-index and merges their results,
// instead of answering from the main index alone.
func TestInterceptWorkspaceSymbol_MergesSubIndexes(t *testing.T) {
	p, _ := newTestProxy()
	clientBuf := &syncBuffer{}
	p.toClient = newFrameWriter(clientBuf)
	p.root = t.TempDir()

	mainRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(mainRoot, "go.mod"), []byte("module example.com/main\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "a.go"), []byte("package a\n\nfunc AlphaOnly() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.idx = newRevIndex(log.New(io.Discard, "", 0))
	p.idx.Build(mainRoot)
	if !p.idx.WaitReady(2 * time.Second) {
		t.Fatal("main index did not become ready")
	}

	subRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(subRoot, "go.mod"), []byte("module example.com/nested\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subRoot, "b.go"), []byte("package b\n\nfunc BetaOnly() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := newRevIndex(log.New(io.Discard, "", 0))
	sub.Build(subRoot)
	if !sub.WaitReady(2 * time.Second) {
		t.Fatal("sub-index did not become ready")
	}
	p.subIdx = map[string]*revIndex{subRoot: sub}

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"workspace/symbol","params":{"query":""}}`)
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !p.interceptWorkspaceSymbol(raw, &m) {
		t.Fatal("interceptWorkspaceSymbol did not take over the request")
	}

	waitFor(t, 2*time.Second, "workspace/symbol response written", func() bool {
		return clientBuf.String() != ""
	})
	out := clientBuf.String()
	if !strings.Contains(out, "AlphaOnly") {
		t.Errorf("response missing main-index symbol AlphaOnly: %s", out)
	}
	if !strings.Contains(out, "BetaOnly") {
		t.Errorf("response missing sub-index symbol BetaOnly: %s", out)
	}
}

// TestEvictIdleModules_DropsIdleModule verifies that a nested module with no
// surviving scope unit and no open document under its root has its subIdx
// entry and graph subslot dropped by the sweep.
func TestEvictIdleModules_DropsIdleModule(t *testing.T) {
	p, _ := newTestProxy()
	root := t.TempDir()
	p.root = root
	modRoot := filepath.Join(root, "wt", "nested")

	p.subIdx = map[string]*revIndex{modRoot: newRevIndex(log.New(io.Discard, "", 0))}
	p.graph = &graphServer{log: log.New(io.Discard, "", 0), subslots: map[string]*graphSlot{modRoot: {}}}

	p.evictIdleModules()

	p.subIdxMu.Lock()
	_, stillIndexed := p.subIdx[modRoot]
	p.subIdxMu.Unlock()
	if stillIndexed {
		t.Error("evictIdleModules did not drop the subIdx entry for an idle module")
	}
	p.graph.mu.Lock()
	_, stillSlotted := p.graph.subslots[modRoot]
	p.graph.mu.Unlock()
	if stillSlotted {
		t.Error("evictIdleModules did not drop the graph subslot for an idle module")
	}
}

// TestEvictIdleModules_KeepsModuleWithOpenDoc verifies the open-docs check is
// authoritative: a module with an open document under its root is never
// dropped, even though it has no scope unit at all.
func TestEvictIdleModules_KeepsModuleWithOpenDoc(t *testing.T) {
	p, _ := newTestProxy()
	root := t.TempDir()
	p.root = root
	modRoot := filepath.Join(root, "wt", "nested")
	openPath := filepath.Join(modRoot, "a.go")
	uri := pathToURI(openPath)
	p.openDocs[uri] = openDoc{URI: uri}

	p.subIdx = map[string]*revIndex{modRoot: newRevIndex(log.New(io.Discard, "", 0))}
	p.graph = &graphServer{log: log.New(io.Discard, "", 0), subslots: map[string]*graphSlot{modRoot: {}}}

	p.evictIdleModules()

	p.subIdxMu.Lock()
	_, stillIndexed := p.subIdx[modRoot]
	p.subIdxMu.Unlock()
	if !stillIndexed {
		t.Error("evictIdleModules dropped the subIdx entry for a module with an open document")
	}
	p.graph.mu.Lock()
	_, stillSlotted := p.graph.subslots[modRoot]
	p.graph.mu.Unlock()
	if !stillSlotted {
		t.Error("evictIdleModules dropped the graph subslot for a module with an open document")
	}
}

// TestEvictIdleModules_KeepsModuleWithScopeUnit verifies a module with a live
// scope unit (prefixed with its root-relative path, per prefixUnit/unitFor)
// is not dropped.
func TestEvictIdleModules_KeepsModuleWithScopeUnit(t *testing.T) {
	p, _ := newTestProxy()
	root := t.TempDir()
	p.root = root
	modRoot := filepath.Join(root, "wt", "nested")
	p.scope["wt/nested/pkg"] = &scopeEntry{open: map[string]bool{}, lastActive: time.Now()}

	p.subIdx = map[string]*revIndex{modRoot: newRevIndex(log.New(io.Discard, "", 0))}
	p.graph = &graphServer{log: log.New(io.Discard, "", 0), subslots: map[string]*graphSlot{modRoot: {}}}

	p.evictIdleModules()

	p.subIdxMu.Lock()
	_, stillIndexed := p.subIdx[modRoot]
	p.subIdxMu.Unlock()
	if !stillIndexed {
		t.Error("evictIdleModules dropped the subIdx entry for a module with a live scope unit")
	}
	p.graph.mu.Lock()
	_, stillSlotted := p.graph.subslots[modRoot]
	p.graph.mu.Unlock()
	if !stillSlotted {
		t.Error("evictIdleModules dropped the graph subslot for a module with a live scope unit")
	}
}

// TestEvictIdleModules_ReTouchRebuildsLazily proves eviction is not a
// permanent break: re-touching an evicted module makes indexFor and the
// graph subslot lookup lazily rebuild it, exactly as for a module never
// before seen.
func TestEvictIdleModules_ReTouchRebuildsLazily(t *testing.T) {
	p, _ := newTestProxy()
	root := t.TempDir()
	p.root = root
	p.idx = newRevIndex(log.New(io.Discard, "", 0))
	p.modRoots = &moduleRootCache{}

	modRoot := filepath.Join(root, "wt", "nested")
	if err := os.MkdirAll(modRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modRoot, "go.mod"), []byte("module example.com/nested\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modRoot, "x.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p.graph = &graphServer{log: log.New(io.Discard, "", 0), root: root, modRoots: p.modRoots, indexFor: p.indexFor}

	// First touch: builds the subIdx entry and the graph subslot.
	first := p.indexFor(modRoot)
	if !first.WaitReady(2 * time.Second) {
		t.Fatal("sub-index did not become ready before eviction")
	}
	firstResp := p.graph.answer(driverQuery{Patterns: nestedModuleLoadPattern, Dir: modRoot, Request: json.RawMessage(`{}`)})
	if bytes.Equal(firstResp, notHandled) {
		t.Fatal("initial nested-module driver query returned NotHandled")
	}

	// Nothing has this module in scope and no document is open: eviction
	// drops both.
	p.evictIdleModules()
	p.subIdxMu.Lock()
	_, stillIndexed := p.subIdx[modRoot]
	p.subIdxMu.Unlock()
	if stillIndexed {
		t.Fatal("evictIdleModules did not drop the subIdx entry")
	}
	p.graph.mu.Lock()
	_, stillSlotted := p.graph.subslots[modRoot]
	p.graph.mu.Unlock()
	if stillSlotted {
		t.Fatal("evictIdleModules did not drop the graph subslot")
	}

	// Re-touch: indexFor must lazily rebuild a working sub-index instance,
	// not return the evicted one.
	second := p.indexFor(modRoot)
	if second == first {
		t.Error("indexFor after eviction returned the evicted instance, want a freshly rebuilt one")
	}
	if !second.WaitReady(2 * time.Second) {
		t.Fatal("re-touched sub-index did not become ready")
	}

	// Re-touch: the graph subslot must lazily rebuild and answer correctly,
	// exactly as it did before eviction.
	secondResp := p.graph.answer(driverQuery{Patterns: nestedModuleLoadPattern, Dir: modRoot, Request: json.RawMessage(`{}`)})
	if bytes.Equal(secondResp, notHandled) {
		t.Error("nested-module driver query after eviction returned NotHandled, want a lazily rebuilt response")
	}
}
