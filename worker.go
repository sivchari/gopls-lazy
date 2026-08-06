package goplslazy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// errWorkerExited marks a worker generation whose process died before or while
// it was serving requests. Callers fall back to the main gopls scope.
var errWorkerExited = errors.New("worker exited")

// workerState is the current, mutable view the worker gopls must mirror: the
// workspace root, the editor's initialize params, the workspace/configuration
// settings, and the set of open documents. It is read fresh on every use (via
// workerClient.stateFn) so replays and configuration answers reflect the
// latest editor state even across worker restarts.
type workerState struct {
	root       string
	initParams json.RawMessage
	config     []json.RawMessage
	docs       []openDoc
}

type workerResponse struct {
	raw []byte
	msg *message
}

// workerHandle owns a single, persistent isolated gopls process that loads the
// whole workspace to answer global queries (implementation, method-wide
// references). The first query on a cold worker is slow, but the process stays
// warm so subsequent queries return in seconds. The handle lazily (re)starts
// the process, replays editor traffic to keep its view fresh, and evicts it
// after an idle TTL.
type workerHandle struct {
	p *proxy

	mu         sync.Mutex
	client     *workerClient
	cmd        *exec.Cmd
	ready      chan struct{} // closed when initialize+replay finishes or fails
	readyDone  bool          // guards a single close of ready per generation
	initErr    error         // non-nil once ready is closed if the worker is unusable
	gen        int           // restart generation; bumped by every spawn
	holds      int           // outstanding request reservations (waiting on ready OR in-flight); the worker is not evicted while > 0
	lastUsed   time.Time
	warned     bool // one "still loading" showMessage per generation
	evictTimer *time.Timer

	// pendingObs buffers editor notifications that arrive while the worker is
	// still initializing (readyDone == false). The open-document set is
	// reconciled by a fresh snapshot at ready time, so replaying these mainly
	// preserves workspace-level events; doc-level ones replay as a harmless
	// refresh. Reset on every spawn.
	pendingObs []bufferedNote
}

// bufferedNote is an editor notification captured during worker initialization
// for replay once the worker goes live.
type bufferedNote struct {
	method string
	params json.RawMessage
}

// ensureStarted returns a live worker client, reusing the current process if
// one is running or spawning a fresh one otherwise. It returns the client, the
// per-generation ready channel to wait on, the generation number, and any spawn
// error. Initialization (initialize + didOpen replay) runs asynchronously; wait
// on the returned channel before issuing requests.
//
// This entry point does NOT reserve the worker against idle eviction: the warm
// path (proxy.patchInitialize) uses it to spawn a worker that stays evictable if
// no query ever arrives. request() uses acquire() instead, which reserves.
func (h *workerHandle) ensureStarted() (*workerClient, chan struct{}, int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ensureStartedLocked()
}

// acquire is ensureStarted plus a reservation: on success it marks the worker as
// held (holds++) and stops the idle-eviction timer, all within the same h.mu
// critical section that returns or creates the client. From the caller's
// perspective the worker is already held the moment acquire returns, closing the
// ensureStarted->hold gap that could let the TTL timer evict a healthy or still
// warming worker. Every successful acquire must be paired with exactly one
// release.
func (h *workerHandle) acquire() (*workerClient, chan struct{}, int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	client, ready, gen, err := h.ensureStartedLocked()
	if err != nil {
		return nil, nil, 0, err
	}
	h.holds++
	h.lastUsed = time.Now()
	h.stopEvictLocked()
	return client, ready, gen, nil
}

// release drops one reservation taken by acquire. When the last reservation is
// dropped it re-arms the idle-eviction timer so an unused worker is eventually
// shut down.
func (h *workerHandle) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holds--
	h.lastUsed = time.Now()
	if h.holds == 0 {
		h.armEvictLocked()
	}
}

// ensureStartedLocked returns the current client if one is live, otherwise
// spawns a fresh worker generation and arms the idle-eviction timer (the timer
// only fires while nothing holds the worker; acquire stops it on reservation).
// Callers must hold h.mu.
func (h *workerHandle) ensureStartedLocked() (*workerClient, chan struct{}, int, error) {
	if h.client != nil {
		return h.client, h.ready, h.gen, nil
	}
	state := h.p.workerState()
	if state.root == "" {
		return nil, nil, 0, fmt.Errorf("workspace root is not initialized")
	}

	cmd := exec.Command(h.p.opts.gopls, h.p.opts.goplsArgs...) //nolint:gosec // gopls path comes from user configuration
	cmd.Env = os.Environ()
	if h.p.opts.driver && h.p.graph != nil {
		if exe, err := os.Executable(); err == nil {
			cmd.Env = append(cmd.Env,
				"GOPACKAGESDRIVER="+exe,
				"GOPLS_LAZY_DRIVER=1",
				"GOPLS_LAZY_SOCK="+h.p.graph.sockPath,
			)
		}
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, 0, err
	}

	h.gen++
	gen := h.gen
	ready := make(chan struct{})
	client := &workerClient{
		stateFn: h.p.workerState,
		in:      newFrameWriter(stdin),
		out:     bufio.NewReaderSize(stdout, 1<<20),
		pending: map[string]chan workerResponse{},
		onDown:  func() { h.onExit(gen) },
	}
	h.client = client
	h.cmd = cmd
	h.ready = ready
	h.readyDone = false
	h.initErr = nil
	h.warned = false
	h.lastUsed = time.Now()
	h.pendingObs = nil

	go client.readLoop()
	go h.initWorker(client, gen)
	go func() { _ = cmd.Wait(); h.onExit(gen) }()
	h.armEvictLocked()
	return client, ready, gen, nil
}

// initWorker runs initialize, then — under h.mu, atomically with going live —
// replays a FRESH snapshot of the open documents and flushes notifications
// buffered during the load before closing the generation's ready channel.
// Taking the snapshot at ready time (rather than once at spawn) guarantees the
// worker's open-document set matches the editor's exactly at the moment it goes
// live, so files opened or edited during the multi-minute cold load are not
// lost. On failure it records the error, clears the (dead) client so the next
// request respawns, and kills the process.
func (h *workerHandle) initWorker(c *workerClient, gen int) {
	err := c.initialize(h.p.opts.workerTimeout)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != gen || h.readyDone {
		return // superseded or already finalized by onExit
	}
	h.initErr = err
	if err != nil {
		cmd := h.cmd
		h.client = nil
		h.cmd = nil
		h.pendingObs = nil
		h.stopEvictLocked()
		h.closeReadyLocked()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return
	}
	h.replayLiveLocked(c)
}

// replayLiveLocked reconciles a freshly-initialized worker's document view with
// the editor's CURRENT state and marks it ready. It re-snapshots the open docs
// (didOpen carries full text, covering both files opened after spawn and edits
// made during the load), flushes any notifications buffered while initializing,
// then closes ready. observe() serializes on h.mu, so a notification is either
// captured by this snapshot (it ran before) or mirrored live (it runs after
// readyDone) — never lost. Callers must hold h.mu.
func (h *workerHandle) replayLiveLocked(c *workerClient) {
	for _, doc := range c.stateFn().docs {
		c.didOpen(doc)
	}
	for _, n := range h.pendingObs {
		h.mirror(c, n.method, n.params)
	}
	h.pendingObs = nil
	h.closeReadyLocked()
}

// request ensures a worker is running, waits up to budget for it to finish
// loading, then forwards the raw request and waits (within the remaining
// budget) for the answer. The worker is reserved for the whole call via
// acquire, so it cannot be idle-evicted while this request waits on the load or
// runs, and the reservation is released exactly once on every return path.
// Exhausting the budget while the worker is still loading returns an error but
// never kills the process: it keeps warming so a retry succeeds.
func (h *workerHandle) request(raw []byte, budget time.Duration) (*message, error) {
	client, ready, gen, err := h.acquire()
	if err != nil {
		return nil, err
	}
	defer h.release()

	deadline := time.Now().Add(budget)
	select {
	case <-ready:
	case <-time.After(budget):
		return nil, fmt.Errorf("worker still loading the workspace after %s (it keeps warming; retry shortly)", budget)
	}

	h.mu.Lock()
	staleGen := h.gen != gen
	initErr := h.initErr
	h.mu.Unlock()
	if staleGen {
		return nil, fmt.Errorf("worker restarted while loading; retry shortly")
	}
	if initErr != nil {
		return nil, initErr
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = time.Second
	}
	return client.request(raw, remaining)
}

// onExit finalizes a worker generation whose process has exited. Stale
// generations (already superseded by a restart) are ignored.
func (h *workerHandle) onExit(gen int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != gen {
		return
	}
	h.client = nil
	h.cmd = nil
	h.stopEvictLocked()
	if !h.readyDone {
		if h.initErr == nil {
			h.initErr = errWorkerExited
		}
		h.closeReadyLocked()
	}
}

// observe mirrors an editor notification onto the worker so its view of the
// workspace tracks the editor's. While the worker is still initializing the
// notification is buffered and replayed at ready time, after a fresh snapshot
// reconciles the open-document set (see replayLiveLocked). It is a genuine
// no-op only when no worker exists.
func (h *workerHandle) observe(method string, params json.RawMessage) {
	if h == nil {
		return
	}
	h.mu.Lock()
	client := h.client
	if client == nil {
		h.mu.Unlock()
		return // no worker: nothing to mirror or reconcile
	}
	if !h.readyDone {
		// Still loading: buffer for replay so state changed during the cold load
		// is not silently lost. Copy params: it aliases the read buffer and is
		// used after this call returns.
		h.pendingObs = append(h.pendingObs, bufferedNote{
			method: method,
			params: append(json.RawMessage(nil), params...),
		})
		h.mu.Unlock()
		return
	}
	initErr := h.initErr
	h.mu.Unlock()
	if initErr != nil {
		return
	}
	h.mirror(client, method, params)
}

// mirror forwards a single editor notification to a live worker client. Callers
// pass the client explicitly and must not hold h.mu, except replayLiveLocked,
// which holds h.mu so its replay writes are ordered strictly before any
// concurrent observe() (which blocks on h.mu until ready).
func (h *workerHandle) mirror(client *workerClient, method string, params json.RawMessage) {
	switch method {
	case methodDidOpen:
		if doc, ok := h.p.trackedDoc(docURI(params)); ok {
			client.didOpen(doc)
		}
	case methodDidChange:
		// Rebuild a full-text didChange from the tracked overlay: range-based
		// edits would desync after a restart replayed didOpen at a fresh
		// version, so always send the whole document at the tracked version.
		if doc, ok := h.p.trackedDoc(docURI(params)); ok {
			client.didChangeFull(doc)
		}
	case "textDocument/didClose":
		client.didClose(docURI(params))
	case methodDidChangeWatchedFiles:
		client.notifyRaw(methodDidChangeWatchedFiles, params)
	case "workspace/didChangeConfiguration":
		// gopls re-pulls workspace/configuration on this; the answer serves the
		// current config via stateFn.
		client.notifyRaw("workspace/didChangeConfiguration", json.RawMessage(`{"settings":{}}`))
	}
}

// shouldWarnLoading reports whether a "still loading" message should be shown to
// the editor: true at most once per worker generation, and only while the
// worker is not yet ready to serve.
func (h *workerHandle) shouldWarnLoading() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil && h.readyDone && h.initErr == nil {
		return false // already loaded
	}
	if h.warned {
		return false
	}
	h.warned = true
	return true
}

// shutdown tears down the running worker (graceful shutdown+exit, then a kill
// backstop). Called from the idle-eviction timer and from proxy.run() on exit.
func (h *workerHandle) shutdown(reason string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	client, cmd := h.client, h.cmd
	if client == nil {
		h.mu.Unlock()
		return
	}
	h.client = nil
	h.cmd = nil
	h.stopEvictLocked()
	if !h.readyDone {
		if h.initErr == nil {
			h.initErr = errWorkerExited
		}
		h.closeReadyLocked()
	}
	h.mu.Unlock()
	if h.p != nil && h.p.log != nil {
		h.p.log.Printf("worker: shutdown (%s)", reason)
	}
	go gracefulStop(client, cmd)
}

// closeReadyLocked closes the current generation's ready channel exactly once.
// Callers must hold h.mu.
func (h *workerHandle) closeReadyLocked() {
	if h.readyDone {
		return
	}
	h.readyDone = true
	close(h.ready)
}

// stopEvictLocked cancels any pending idle-eviction timer. Callers must hold
// h.mu.
func (h *workerHandle) stopEvictLocked() {
	if h.evictTimer != nil {
		h.evictTimer.Stop()
		h.evictTimer = nil
	}
}

// armEvictLocked schedules an idle-eviction check after the configured TTL.
// Callers must hold h.mu. A zero TTL disables eviction, and it is a no-op when
// no worker is running. The timer only shuts the worker down if nothing holds
// it when it fires (see evictIfIdle), so it is armed at spawn and whenever the
// last reservation is released, and stopped when a reservation is taken.
func (h *workerHandle) armEvictLocked() {
	h.stopEvictLocked()
	ttl := h.p.opts.workerTTL
	if ttl <= 0 || h.client == nil {
		return
	}
	gen := h.gen
	h.evictTimer = time.AfterFunc(ttl, func() { h.evictIfIdle(gen) })
}

// evictIfIdle shuts the worker down if it is still the current generation and
// nothing holds it (no request is waiting on the load or in-flight).
func (h *workerHandle) evictIfIdle(gen int) {
	h.mu.Lock()
	idle := h.gen == gen && h.client != nil && h.holds == 0
	h.mu.Unlock()
	if !idle {
		return
	}
	h.shutdown(fmt.Sprintf("idle for %s", h.p.opts.workerTTL))
}

// gracefulStop asks a worker gopls to shut down and exit, then kills it after a
// short grace period. It is a no-op for a client with no process (unit tests).
func gracefulStop(c *workerClient, cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = c.call("shutdown", nil, 2*time.Second)
		c.notify("exit", struct{}{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	time.AfterFunc(2*time.Second, func() { _ = cmd.Process.Kill() })
}

// workerState snapshots the current editor-visible state for a worker gopls.
func (p *proxy) workerState() workerState {
	p.mu.Lock()
	defer p.mu.Unlock()
	docs := make([]openDoc, 0, len(p.openDocs))
	for _, doc := range p.openDocs {
		docs = append(docs, doc)
	}
	config := append([]json.RawMessage(nil), p.workerConfig...)
	return workerState{
		root:       p.root,
		initParams: append(json.RawMessage(nil), p.workerInit...),
		config:     config,
		docs:       docs,
	}
}

// trackedDoc returns the current overlay for an open document, if any.
func (p *proxy) trackedDoc(uri string) (openDoc, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	doc, ok := p.openDocs[uri]
	return doc, ok
}

type workerClient struct {
	stateFn func() workerState
	in      *frameWriter
	out     *bufio.Reader
	onDown  func()

	mu      sync.Mutex
	seq     int
	pending map[string]chan workerResponse
}

func (c *workerClient) initialize(budget time.Duration) error {
	st := c.stateFn()
	params := st.initParams
	if len(params) == 0 {
		params = defaultInitializeParams(st.root)
	}
	if err := c.call("initialize", params, budget); err != nil {
		return err
	}
	c.notify("initialized", struct{}{})
	return nil
}

func defaultInitializeParams(root string) json.RawMessage {
	uri := pathToURI(root)
	name := filepath.Base(root)
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   uri,
		"capabilities": map[string]any{
			"workspace": map[string]any{"configuration": true},
		},
		"workspaceFolders": []workspaceFolder{{URI: uri, Name: name}},
	}
	raw, _ := json.Marshal(params)
	return raw
}

func (c *workerClient) didOpen(doc openDoc) {
	c.notify(methodDidOpen, didOpenParams{
		TextDocument: textDocumentItem(doc),
	})
}

// didChangeFull sends a range-less (full-text) didChange from the tracked
// overlay, immune to version drift after a restart replayed didOpen.
func (c *workerClient) didChangeFull(doc openDoc) {
	c.notify(methodDidChange, didChangeParams{
		TextDocument:   versionedTextDocumentIdentifier{URI: doc.URI, Version: doc.Version},
		ContentChanges: []contentChange{{Text: doc.Text}},
	})
}

func (c *workerClient) didClose(uri string) {
	if uri == "" {
		return
	}
	c.notify("textDocument/didClose", didCloseParams{
		TextDocument: textDocumentIdentifier{URI: uri},
	})
}

// notify sends a parameterized JSON-RPC notification to the worker gopls.
func (c *workerClient) notify(method string, params any) {
	var raw json.RawMessage
	if b, err := json.Marshal(params); err == nil {
		raw = b
	}
	c.notifyRaw(method, raw)
}

// notifyRaw sends a JSON-RPC notification with pre-encoded params.
func (c *workerClient) notifyRaw(method string, params json.RawMessage) {
	msg := message{JSONRPC: jsonrpcVersion, Method: method, Params: params}
	if b, err := json.Marshal(msg); err == nil {
		c.in.write(b)
	}
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type contentChange struct {
	Text string `json:"text"`
}

type didChangeParams struct {
	TextDocument   versionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []contentChange                 `json:"contentChanges"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

func (c *workerClient) request(raw []byte, timeout time.Duration) (*message, error) {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.ID == nil {
		return nil, fmt.Errorf("request has no id")
	}
	ch := c.register(string(m.ID))
	c.in.write(raw)
	select {
	case resp := <-ch:
		if resp.msg == nil {
			return nil, fmt.Errorf("%s: worker connection closed", m.Method)
		}
		return resp.msg, nil
	case <-time.After(timeout):
		c.unregister(string(m.ID))
		return nil, fmt.Errorf("%s timed out after %s", m.Method, timeout)
	}
}

// call sends a server-bound request (initialize, shutdown) and waits for its
// response within timeout. The response body is not returned because callers
// only care whether the call succeeded.
func (c *workerClient) call(method string, params json.RawMessage, timeout time.Duration) error {
	c.mu.Lock()
	c.seq++
	id := fmt.Sprintf(`"worker-%d"`, c.seq)
	ch := make(chan workerResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(id),
		Method:  method,
		Params:  params,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.in.write(raw)

	select {
	case resp := <-ch:
		if resp.msg == nil {
			return fmt.Errorf("%s: worker connection closed", method)
		}
		if len(resp.msg.Error) > 0 {
			return fmt.Errorf("%s failed: %s", method, string(resp.msg.Error))
		}
		return nil
	case <-time.After(timeout):
		c.unregister(id)
		return fmt.Errorf("%s timed out", method)
	}
}

func (c *workerClient) readLoop() {
	for {
		raw, err := readFrame(c.out)
		if err != nil {
			c.failPending()
			if c.onDown != nil {
				c.onDown()
			}
			return
		}
		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Method == "" && m.ID != nil {
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- workerResponse{raw: raw, msg: &m}
			}
			continue
		}
		if m.ID != nil {
			c.respondToServerRequest(&m)
		}
	}
}

// failPending delivers a closed-connection sentinel to every waiter so
// in-flight requests fail fast when the worker process dies, instead of
// blocking until their individual timeouts.
func (c *workerClient) failPending() {
	c.mu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		select {
		case ch <- workerResponse{}:
		default:
		}
	}
	c.mu.Unlock()
}

func (c *workerClient) respondToServerRequest(m *message) {
	result := json.RawMessage(`null`)
	if m.Method == methodConfiguration {
		result = c.configurationResult(m.Params)
	}
	resp := message{
		JSONRPC: jsonrpcVersion,
		ID:      m.ID,
		Result:  result,
	}
	raw, err := json.Marshal(resp)
	if err == nil {
		c.in.write(raw)
	}
}

// configurationResult answers workspace/configuration with exactly one settings
// object per requested item, serving the current config captured by stateFn.
func (c *workerClient) configurationResult(params json.RawMessage) json.RawMessage {
	config := c.stateFn().config
	var cp struct {
		Items []struct {
			Section string `json:"section"`
		} `json:"items"`
	}
	count := len(config)
	if json.Unmarshal(params, &cp) == nil && len(cp.Items) > 0 {
		count = len(cp.Items)
	}
	items := make([]json.RawMessage, count)
	for i := range items {
		if i < len(config) && len(config[i]) > 0 {
			items[i] = config[i]
		} else {
			items[i] = json.RawMessage(`{}`)
		}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return raw
}

func (c *workerClient) register(id string) chan workerResponse {
	ch := make(chan workerResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

func (c *workerClient) unregister(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
