package goplslazy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"testing"
	"time"
)

func newTestWorkerProxy(ttl time.Duration) *proxy {
	return &proxy{
		opts: options{workerTTL: ttl, workerTimeout: time.Minute},
		log:  log.New(io.Discard, "", 0),
	}
}

func TestWorkerClient_ConfigurationResult(t *testing.T) {
	config := []json.RawMessage{json.RawMessage(`{"a":1}`), json.RawMessage(`{"b":2}`)}
	c := &workerClient{stateFn: func() workerState { return workerState{config: config} }}
	tests := []struct {
		name    string
		params  string
		wantLen int
	}{
		{"one settings object per requested item", `{"items":[{"section":"gopls"},{"section":"gopls"},{"section":"gopls"}]}`, 3},
		{"falls back to config length when no items", `{}`, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := c.configurationResult(json.RawMessage(tt.params))
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil {
				t.Fatalf("result not a JSON array: %v (%s)", err, raw)
			}
			if len(items) != tt.wantLen {
				t.Fatalf("configurationResult returned %d items, want %d", len(items), tt.wantLen)
			}
			// The first item must carry the current config, not an empty stub.
			if string(items[0]) != string(config[0]) {
				t.Errorf("item[0] = %s, want current config %s", items[0], config[0])
			}
		})
	}
}

func TestWorkerClient_DidChangeFull(t *testing.T) {
	var buf bytes.Buffer
	c := &workerClient{in: newFrameWriter(&buf)}
	c.didChangeFull(openDoc{URI: "file:///x.go", Version: 7, Text: "package x\n"})

	frame, err := readFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m message
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if m.Method != "textDocument/didChange" {
		t.Fatalf("method = %q, want textDocument/didChange", m.Method)
	}
	var p struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Range *lspRange `json:"range"`
			Text  string    `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if p.TextDocument.Version != 7 {
		t.Errorf("version = %d, want 7 (from tracked overlay)", p.TextDocument.Version)
	}
	if len(p.ContentChanges) != 1 {
		t.Fatalf("contentChanges = %d, want 1 full-text change", len(p.ContentChanges))
	}
	if p.ContentChanges[0].Range != nil {
		t.Errorf("full-text change must be range-less, got range %+v", p.ContentChanges[0].Range)
	}
	if p.ContentChanges[0].Text != "package x\n" {
		t.Errorf("text = %q, want full document text", p.ContentChanges[0].Text)
	}
}

func TestRequestFailedError(t *testing.T) {
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(requestFailedError("boom"), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Code != codeRequestFailed {
		t.Errorf("code = %d, want %d", e.Code, codeRequestFailed)
	}
	if e.Message != "boom" {
		t.Errorf("message = %q, want boom", e.Message)
	}
}

func TestWorkerHandle_OnExitStaleGen(t *testing.T) {
	h := &workerHandle{
		p:      newTestWorkerProxy(0),
		client: &workerClient{},
		gen:    5,
		ready:  make(chan struct{}),
	}

	h.onExit(3) // stale generation: must not touch the live worker
	if h.client == nil {
		t.Fatal("stale onExit cleared the current client")
	}

	h.onExit(5) // current generation: finalizes the worker
	if h.client != nil {
		t.Error("current onExit did not clear the client")
	}
	if !h.readyDone {
		t.Error("current onExit did not close ready")
	}
	if h.initErr == nil {
		t.Error("current onExit did not record an init error")
	}
}

func TestWorkerHandle_EvictionTimer(t *testing.T) {
	t.Run("idle worker is evicted after TTL", func(t *testing.T) {
		h := &workerHandle{
			p:         newTestWorkerProxy(15 * time.Millisecond),
			client:    &workerClient{},
			cmd:       nil, // no process: gracefulStop is a no-op
			gen:       1,
			ready:     make(chan struct{}),
			readyDone: true,
			lastUsed:  time.Now(),
		}
		h.mu.Lock()
		h.armEvictLocked()
		h.mu.Unlock()

		waitFor(t, time.Second, "idle worker evicted", func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.client == nil
		})
	})

	t.Run("busy worker is not evicted", func(t *testing.T) {
		h := &workerHandle{
			p:         newTestWorkerProxy(15 * time.Millisecond),
			client:    &workerClient{},
			gen:       1,
			ready:     make(chan struct{}),
			readyDone: true,
			holds:     1,
			lastUsed:  time.Now(),
		}
		h.mu.Lock()
		h.armEvictLocked()
		h.mu.Unlock()

		time.Sleep(60 * time.Millisecond)
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.client == nil {
			t.Error("worker with an in-flight request was evicted")
		}
	})

	t.Run("worker is not evicted while a request waits on the cold load", func(t *testing.T) {
		// Finding 1: a request reserves the worker before waiting on ready, so
		// even a still-loading worker (ready never closed) survives the TTL.
		h := &workerHandle{
			p:      newTestWorkerProxy(15 * time.Millisecond),
			client: &workerClient{},
			gen:    1,
			ready:  make(chan struct{}), // never closed: still warming
			holds:  1,                   // a request is waiting on the load
		}
		h.mu.Lock()
		h.armEvictLocked()
		h.mu.Unlock()

		time.Sleep(50 * time.Millisecond)
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.client == nil {
			t.Error("worker with a request waiting on the cold load was evicted mid-load")
		}
	})

	t.Run("zero TTL disables eviction", func(t *testing.T) {
		h := &workerHandle{
			p:         newTestWorkerProxy(0),
			client:    &workerClient{},
			gen:       1,
			ready:     make(chan struct{}),
			readyDone: true,
		}
		h.mu.Lock()
		h.armEvictLocked()
		armed := h.evictTimer != nil
		h.mu.Unlock()
		if armed {
			t.Error("eviction timer armed with TTL=0")
		}
	})
}

// TestWorkerHandle_AcquireRelease exercises the reservation counter that keeps a
// worker alive across the whole request (waiting on the load and in-flight), and
// the arm/stop transitions of the idle-eviction timer as holds go 0->1->0.
func TestWorkerHandle_AcquireRelease(t *testing.T) {
	h := &workerHandle{
		p:         newTestWorkerProxy(15 * time.Millisecond),
		client:    &workerClient{}, // live: acquire reuses it instead of spawning
		gen:       1,
		ready:     make(chan struct{}),
		readyDone: true,
	}
	// Start idle with the evict timer armed, as after a spawn with no holds.
	h.mu.Lock()
	h.armEvictLocked()
	h.mu.Unlock()

	// acquire reserves the worker and stops the timer, atomically.
	client, _, gen, err := h.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if client == nil {
		t.Fatal("acquire returned a nil client for a live worker")
	}
	h.mu.Lock()
	if h.holds != 1 {
		t.Errorf("holds after acquire = %d, want 1", h.holds)
	}
	if h.evictTimer != nil {
		t.Error("acquire did not stop the idle-eviction timer")
	}
	h.mu.Unlock()

	// While held, eviction must not fire even well past the TTL.
	time.Sleep(40 * time.Millisecond)
	h.mu.Lock()
	if h.client == nil {
		t.Error("held worker was evicted")
	}
	h.mu.Unlock()

	// release drops the reservation and re-arms eviction.
	h.release()
	h.mu.Lock()
	if h.holds != 0 {
		t.Errorf("holds after release = %d, want 0", h.holds)
	}
	if h.evictTimer == nil {
		t.Error("release did not re-arm the idle-eviction timer")
	}
	h.mu.Unlock()
	_ = gen

	// With no holds the idle worker is now evicted after the TTL.
	waitFor(t, time.Second, "released idle worker evicted", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.client == nil
	})
}

// TestWorkerHandle_ReplayLiveResnapshotsDocs verifies that going live
// re-snapshots the CURRENT open documents (so a file added or edited during the
// cold load is didOpen'd) and flushes buffered workspace notifications.
func TestWorkerHandle_ReplayLiveResnapshotsDocs(t *testing.T) {
	var buf bytes.Buffer
	// docA was open at spawn; docB simulates a file opened (or edited) AFTER the
	// spawn-time view — it is only present because we snapshot at ready time.
	docA := openDoc{URI: "file:///a.go", LanguageID: "go", Version: 1, Text: "package a\n"}
	docB := openDoc{URI: "file:///b.go", LanguageID: "go", Version: 3, Text: "package b\n// edited during load\n"}
	p := &proxy{
		opts:     options{workerTTL: 0},
		openDocs: map[string]openDoc{docA.URI: docA, docB.URI: docB},
		log:      log.New(io.Discard, "", 0),
	}
	c := &workerClient{stateFn: p.workerState, in: newFrameWriter(&buf)}
	h := &workerHandle{p: p, client: c, gen: 1, ready: make(chan struct{})}

	// A workspace-level notification arrived while initializing; it must survive.
	watched := json.RawMessage(`{"changes":[{"uri":"file:///a.go","type":2}]}`)
	h.mu.Lock()
	h.pendingObs = []bufferedNote{{method: methodDidChangeWatchedFiles, params: watched}}
	h.replayLiveLocked(c)
	h.mu.Unlock()

	if !h.readyDone {
		t.Fatal("replayLiveLocked did not mark the worker ready")
	}
	if h.pendingObs != nil {
		t.Error("replayLiveLocked did not drain the buffered notifications")
	}

	msgs := drainFrames(t, &buf)
	openedText := map[string]string{}
	watchedReplayed := false
	for _, m := range msgs {
		switch m.Method {
		case methodDidOpen:
			var dp didOpenParams
			if err := json.Unmarshal(m.Params, &dp); err != nil {
				t.Fatalf("unmarshal didOpen: %v", err)
			}
			openedText[dp.TextDocument.URI] = dp.TextDocument.Text
		case methodDidChangeWatchedFiles:
			watchedReplayed = true
		}
	}
	if _, ok := openedText[docA.URI]; !ok {
		t.Errorf("didOpen missing for %s", docA.URI)
	}
	if got := openedText[docB.URI]; got != docB.Text {
		t.Errorf("didOpen for doc added during load = %q, want full edited text %q", got, docB.Text)
	}
	if !watchedReplayed {
		t.Error("buffered workspace/didChangeWatchedFiles was not replayed at ready time")
	}
}

// TestWorkerHandle_ObserveBuffersWhileLoading checks that observe buffers
// notifications while the worker is initializing (so state is not lost) and is a
// genuine no-op only when no worker exists.
func TestWorkerHandle_ObserveBuffersWhileLoading(t *testing.T) {
	p := &proxy{
		opts:     options{workerTTL: 0},
		openDocs: map[string]openDoc{},
		log:      log.New(io.Discard, "", 0),
	}
	h := &workerHandle{p: p, gen: 1, ready: make(chan struct{})}

	// No worker yet: observe is a genuine no-op, nothing buffered.
	h.observe(methodDidChangeWatchedFiles, json.RawMessage(`{}`))
	h.mu.Lock()
	if len(h.pendingObs) != 0 {
		t.Errorf("observe buffered %d notes with no worker, want 0", len(h.pendingObs))
	}
	h.mu.Unlock()

	// Worker spawned but still loading (readyDone false): observe buffers.
	h.mu.Lock()
	h.client = &workerClient{stateFn: p.workerState}
	h.mu.Unlock()
	h.observe(methodDidChangeWatchedFiles, json.RawMessage(`{"changes":[]}`))
	h.mu.Lock()
	got := len(h.pendingObs)
	h.mu.Unlock()
	if got != 1 {
		t.Errorf("observe buffered %d notes while loading, want 1", got)
	}
}

// drainFrames reads every LSP frame remaining in buf and decodes each as a
// message. It stops at EOF.
func drainFrames(t *testing.T, buf *bytes.Buffer) []message {
	t.Helper()
	var msgs []message
	r := bufio.NewReader(buf)
	for {
		frame, err := readFrame(r)
		if err != nil {
			break
		}
		var m message
		if err := json.Unmarshal(frame, &m); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}
