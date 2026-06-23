package goplslazy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// scopeEntry tracks why a unit is in scope: the open files inside it, and
// when it was last useful, so idle units can be evicted.
type scopeEntry struct {
	open       map[string]bool
	lastActive time.Time
}

type proxy struct {
	opts options

	mu           sync.Mutex
	root         string                 // workspace root filesystem path
	scope        map[string]*scopeEntry // scope units relative to root
	userFilters  []string               // editor's own directoryFilters, restored for root-level files
	configIDs    map[string]bool        // ids of pending server->client workspace/configuration requests
	workerInit   json.RawMessage        // original initialize params for isolated worker gopls processes
	workerConfig []json.RawMessage      // original workspace/configuration settings, before lazy filters
	openDocs     map[string]openDoc     // current open document overlays for worker gopls processes
	pendingDiag  map[string]bool        // opened uris whose first diagnostics have not arrived yet
	rescopeTimer *time.Timer
	held         [][]byte // requests waiting for the re-scoped view
	awaitingLoad bool     // a held-triggered rescope is in flight
	holdTimer    *time.Timer
	pendingOwn   map[string]chan *message // proxy-originated request ids -> response channel
	ownSeq       int

	workerSF singleflight.Group // dedups concurrent identical isolated-worker requests

	idx   *revIndex
	graph *graphServer

	toServer *frameWriter
	toClient *frameWriter
	log      *log.Logger
}

func (p *proxy) run() int {
	args := p.opts.goplsArgs
	cmd := exec.Command(p.opts.gopls, args...) //nolint:gosec // gopls path comes from user configuration, not untrusted input
	cmd.Env = os.Environ()
	if p.opts.driver {
		g, err := startGraphServer(p.idx, p.log)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gopls-lazy: graph server: %v (continuing without driver)\n", err)
		} else {
			p.graph = g
			exe, err := os.Executable()
			if err == nil {
				cmd.Env = append(cmd.Env,
					"GOPACKAGESDRIVER="+exe,
					"GOPLS_LAZY_DRIVER=1",
					"GOPLS_LAZY_SOCK="+g.sockPath,
				)
			}
		}
	}
	cmd.Stderr = os.Stderr
	serverIn, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-lazy: %v\n", err)
		return 1
	}
	serverOut, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-lazy: %v\n", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gopls-lazy: start gopls: %v\n", err)
		return 1
	}
	p.toServer = newFrameWriter(serverIn)
	p.toClient = newFrameWriter(os.Stdout)

	if p.opts.evictTTL > 0 {
		go p.evictLoop()
	}

	done := make(chan struct{}, 2)
	go func() { p.pumpClient(bufio.NewReaderSize(os.Stdin, 1<<20)); done <- struct{}{} }()
	go func() { p.pumpServer(bufio.NewReaderSize(serverOut, 1<<20)); done <- struct{}{} }()
	<-done
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return 0
}

// pumpClient forwards editor->gopls traffic, patching initialize options and
// configuration responses, growing the scope on didOpen, and holding
// cross-reference requests until the scope covers their target.
func (p *proxy) pumpClient(r *bufio.Reader) {
	for {
		raw, err := readFrame(r)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw, &m) != nil {
			p.toServer.write(raw)
			continue
		}
		if m.ID != nil && p.interceptClientRequest(raw, &m) {
			continue // held or answered by the proxy
		}
		switch m.Method {
		case "initialize":
			raw = p.patchInitialize(raw, &m)
		case "textDocument/didOpen":
			p.trackDidOpen(m.Params)
			p.observeOpen(m.Params)
		case "textDocument/didChange":
			p.trackDidChange(m.Params)
		case "textDocument/didClose":
			p.trackDidClose(m.Params)
			p.observeClose(m.Params)
		case "textDocument/didSave":
			p.observeFileEvent(docURI(m.Params))
		case "workspace/didChangeWatchedFiles":
			p.observeWatchedFiles(m.Params)
		case "":
			raw = p.patchClientResponse(raw, &m)
		}
		p.toServer.write(raw)
	}
}

// interceptClientRequest takes over editor requests the proxy answers itself
// (workspace symbols) or holds until the scope is wide enough (cross-refs).
// It returns true when the request was swallowed and must not be forwarded.
func (p *proxy) interceptClientRequest(raw []byte, m *message) bool {
	switch {
	case m.Method == "workspace/symbol":
		return p.interceptWorkspaceSymbol(raw, m)
	case isCrossRef(m.Method):
		return p.interceptCrossRef(raw, m)
	default:
		return false
	}
}

// patchClientResponse patches an editor->gopls response if it answers a
// workspace/configuration request the proxy saw going out.
func (p *proxy) patchClientResponse(raw []byte, m *message) []byte {
	if m.ID == nil {
		return raw
	}
	p.mu.Lock()
	isConfig := p.configIDs[string(m.ID)]
	delete(p.configIDs, string(m.ID))
	p.mu.Unlock()
	if isConfig {
		return p.patchConfigResponse(raw, m)
	}
	return raw
}

// pumpServer forwards gopls->editor traffic. It remembers ids of
// workspace/configuration requests, routes responses to proxy-originated
// requests, fires the second stage of a two-stage rescope once orphan
// diagnostics arrive, and releases held requests when the re-scoped
// metadata load completes.
func (p *proxy) pumpServer(r *bufio.Reader) {
	for {
		raw, err := readFrame(r)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw, &m) == nil {
			if m.Method == "" && m.ID != nil {
				p.mu.Lock()
				ch := p.pendingOwn[string(m.ID)]
				delete(p.pendingOwn, string(m.ID))
				p.mu.Unlock()
				if ch != nil {
					ch <- &m
					continue // proxy-originated; not for the editor
				}
			}
			switch m.Method {
			case "workspace/configuration":
				if m.ID != nil {
					p.mu.Lock()
					p.configIDs[string(m.ID)] = true
					p.mu.Unlock()
				}
			case "textDocument/publishDiagnostics":
				p.onDiagnostics(m.Params)
			case "window/logMessage":
				p.onLogMessage(m.Params)
			}
		}
		p.toClient.write(raw)
	}
}

func isCrossRef(method string) bool {
	switch method {
	case methodRename, "textDocument/prepareRename",
		methodReferences, methodImplementation:
		return true
	}
	return false
}

func (p *proxy) interceptWorkspaceSymbol(raw []byte, m *message) bool {
	var params struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(m.Params, &params) != nil {
		return false
	}
	p.mu.Lock()
	hasRoot := p.root != ""
	p.mu.Unlock()
	if !hasRoot {
		return false
	}
	id := append(json.RawMessage(nil), m.ID...)
	query := params.Query
	held := append([]byte(nil), raw...)
	go func() {
		if !p.idx.WaitReady(10 * time.Second) {
			p.log.Printf("workspace/symbol: index not ready in time, forwarding to gopls")
			p.toServer.write(held)
			return
		}
		symbols := p.idx.WorkspaceSymbols(query)
		p.respond(id, symbols)
		p.log.Printf("workspace/symbol: query=%q results=%d", query, len(symbols))
	}()
	return true
}

func (p *proxy) respond(id json.RawMessage, result any) {
	b, err := json.Marshal(result)
	if err != nil {
		return
	}
	resp := message{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Result:  b,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	p.toClient.write(raw)
}

// respondError sends a JSON-RPC error response carrying the given error object
// back to the editor under the supplied request id.
func (p *proxy) respondError(id, lspError json.RawMessage) {
	resp := message{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   lspError,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	p.toClient.write(raw)
}

func (p *proxy) patchInitialize(raw []byte, m *message) []byte {
	var params map[string]any
	if json.Unmarshal(m.Params, &params) != nil {
		return raw
	}
	root := ""
	if uri, _ := params["rootUri"].(string); uri != "" {
		root = uriToPath(uri)
	} else if folders, _ := params["workspaceFolders"].([]any); len(folders) > 0 {
		if f, _ := folders[0].(map[string]any); f != nil {
			if uri, _ := f["uri"].(string); uri != "" {
				root = uriToPath(uri)
			}
		}
	}
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()
	if root != "" {
		go p.idx.Build(root)
		if p.graph != nil {
			// Set root so the disk cache is ready before gopls's first
			// GOPACKAGESDRIVER call during IWL.
			p.graph.setRoot(root)
		}
		// Restore the previous session's scope BEFORE computing the initial
		// filters below, so gopls starts loading those packages during IWL
		// rather than waiting for the first didOpen.  This eliminates the
		// "module '.' not in workspace" orphan error the user would otherwise
		// see during the type-check warmup window.
		p.restoreScope(root)
	}
	opts, _ := params["initializationOptions"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	p.captureUserFilters(opts)
	workerParams := cloneMap(params)
	workerParams["initializationOptions"] = cloneMap(opts)
	if b, err := json.Marshal(workerParams); err == nil {
		p.mu.Lock()
		p.workerInit = b
		p.mu.Unlock()
	}
	p.mu.Lock()
	setFilters(opts, p.filtersLocked())
	p.mu.Unlock()
	params["initializationOptions"] = opts
	patched, err := json.Marshal(params)
	if err != nil {
		return raw
	}
	m.Params = patched
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	p.log.Printf("initialize: root=%s filters=%v", root, opts["directoryFilters"])
	return out
}

// patchConfigResponse injects the current directoryFilters into every
// settings object the editor returns for workspace/configuration, keeping
// all other user settings intact.
func (p *proxy) patchConfigResponse(raw []byte, m *message) []byte {
	var items []any
	if json.Unmarshal(m.Result, &items) != nil {
		return raw
	}
	workerConfig := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		if obj, _ := it.(map[string]any); obj != nil {
			p.captureUserFilters(obj)
		}
		if b, err := json.Marshal(it); err == nil {
			workerConfig = append(workerConfig, b)
		} else {
			workerConfig = append(workerConfig, json.RawMessage(`{}`))
		}
	}
	p.mu.Lock()
	p.workerConfig = workerConfig
	p.mu.Unlock()
	p.mu.Lock()
	fs := p.filtersLocked()
	p.mu.Unlock()
	for i, it := range items {
		obj, _ := it.(map[string]any)
		if obj == nil {
			obj = map[string]any{}
		}
		setFilters(obj, fs)
		items[i] = obj
	}
	patched, err := json.Marshal(items)
	if err != nil {
		return raw
	}
	m.Result = patched
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// captureUserFilters remembers the editor's own directoryFilters so they can
// be restored when a root-level Go file forces the proxy to disable lazy
// filters.
func (p *proxy) captureUserFilters(settings map[string]any) {
	v, ok := settings["directoryFilters"]
	if !ok {
		return
	}
	arr, ok := v.([]any)
	if !ok {
		return
	}
	var fs []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			fs = append(fs, s)
		}
	}
	if len(fs) > 0 && fs[0] != filterExcludeAll { // ignore our own injected value
		p.mu.Lock()
		p.userFilters = fs
		p.mu.Unlock()
	}
}

func docURI(params json.RawMessage) string {
	var dp struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if json.Unmarshal(params, &dp) != nil {
		return ""
	}
	return dp.TextDocument.URI
}

// observeOpen grows the scope for a newly opened file.
//
// Two-stage rescope: when there is already something in scope, gopls can
// serve the file in orphan mode (fast syntax diagnostics without loading the
// package). The second-stage rescope fires after those diagnostics arrive so
// the view reload never blocks the first feedback. A 3-second fallback timer
// handles files that produce no orphan diagnostics (e.g. generated stubs).
//
// First-file shortcut: when the scope is empty the workspace filter is
// "-**", which excludes everything. Gopls produces no orphan diagnostics in
// this state, so waiting for them would add a 3-second dead zone before the
// real type-check even starts. We skip the wait and push the rescope after
// a single debounce tick instead, which batches multiple simultaneous opens.
func (p *proxy) observeOpen(params json.RawMessage) {
	uri := docURI(params)
	path := uriToPath(uri)
	if !strings.HasSuffix(path, ".go") {
		return
	}
	unit, ok := p.unitFor(path)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.scope[unit]; e != nil {
		e.open[uri] = true
		e.lastActive = time.Now()
		return
	}
	wasEmpty := len(p.scope) == 0
	p.scope[unit] = &scopeEntry{open: map[string]bool{uri: true}, lastActive: time.Now()}
	if wasEmpty {
		// Scope was empty: skip two-stage, rescope after one debounce tick
		// so simultaneous opens are still batched.
		p.log.Printf("scope += %s (first open, immediate rescope)", unit)
		if p.rescopeTimer != nil {
			p.rescopeTimer.Stop()
		}
		p.rescopeTimer = time.AfterFunc(p.opts.debounce, p.pushScope)
	} else {
		p.pendingDiag[uri] = true
		p.log.Printf("scope += %s (open, waiting for orphan diagnostics)", unit)
		uriCopy := uri
		time.AfterFunc(3*time.Second, func() { p.diagArrived(uriCopy, "timeout") })
	}
}

func (p *proxy) observeClose(params json.RawMessage) {
	uri := docURI(params)
	path := uriToPath(uri)
	unit, ok := p.unitFor(path)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.scope[unit]; e != nil {
		delete(e.open, uri)
		e.lastActive = time.Now()
	}
}

// evictLoop drops scope units that have had no open files for the TTL.
func (p *proxy) evictLoop() {
	ttl := p.opts.evictTTL
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for range tick.C {
		now := time.Now()
		p.mu.Lock()
		changed := false
		for unit, e := range p.scope {
			if len(e.open) == 0 && now.Sub(e.lastActive) > ttl {
				delete(p.scope, unit)
				p.log.Printf("scope -= %s (idle %s)", unit, ttl)
				changed = true
			}
		}
		p.mu.Unlock()
		if changed {
			p.pushScope()
		}
	}
}

func (p *proxy) onDiagnostics(params json.RawMessage) {
	var dp struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(params, &dp) != nil {
		return
	}
	p.diagArrived(dp.URI, "diagnostics")
}

func (p *proxy) diagArrived(uri, why string) {
	p.mu.Lock()
	if !p.pendingDiag[uri] {
		p.mu.Unlock()
		return
	}
	delete(p.pendingDiag, uri)
	p.log.Printf("second-stage rescope scheduled (%s for %s)", why, uri)
	if p.rescopeTimer != nil {
		p.rescopeTimer.Stop()
	}
	p.rescopeTimer = time.AfterFunc(p.opts.debounce, p.pushScope)
	p.mu.Unlock()
}

// onLogMessage watches gopls's own log for the go/packages.Load line that
// marks the end of a metadata reload, releasing held requests.
func (p *proxy) onLogMessage(params json.RawMessage) {
	var lp struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(params, &lp) != nil {
		return
	}
	p.mu.Lock()
	waiting := p.awaitingLoad
	p.mu.Unlock()
	if waiting && strings.Contains(lp.Message, "go/packages.Load") {
		// Give gopls a beat to swap in the new snapshot.
		time.AfterFunc(300*time.Millisecond, func() { p.flushHeld("load complete") })
	}
}

func (p *proxy) flushHeld(why string) {
	p.mu.Lock()
	held := p.held
	p.held = nil
	p.awaitingLoad = false
	if p.holdTimer != nil {
		p.holdTimer.Stop()
		p.holdTimer = nil
	}
	p.mu.Unlock()
	if len(held) == 0 {
		return
	}
	p.log.Printf("releasing %d held request(s): %s", len(held), why)
	for _, raw := range held {
		p.toServer.write(raw)
	}
}

// observeFileEvent keeps the reverse-import index and the package-graph
// cache in sync with on-disk changes (saves, git operations seen by the
// editor's file watcher).
func (p *proxy) observeFileEvent(uri string) {
	path := uriToPath(uri)
	if path == "" {
		return
	}
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	if root == "" || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return
	}
	base := filepath.Base(path)
	if base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum" {
		if p.graph != nil {
			p.graph.MarkStale("module file changed: " + base)
		}
		return
	}
	if !strings.HasSuffix(path, ".go") {
		// A non-Go file affects the package graph only when it is an //go:embed
		// asset. Invalidating on every non-Go change (build output, generated
		// JSON, editor temp files) would fire a full `go list ./...` rebuild on
		// noise and starve type-checking, so check the embed footprint first.
		if p.graph != nil && p.graph.IsEmbedFile(path) {
			p.graph.MarkStale("embedded asset changed: " + base)
		}
		return
	}
	go func() {
		changed := p.idx.UpdateFile(path)
		if changed && p.graph != nil {
			p.graph.MarkStale("imports or embed directives changed: " + path)
		}
	}()
}

func (p *proxy) observeWatchedFiles(params json.RawMessage) {
	var wp struct {
		Changes []struct {
			URI string `json:"uri"`
		} `json:"changes"`
	}
	if json.Unmarshal(params, &wp) != nil {
		return
	}
	for _, ch := range wp.Changes {
		p.observeFileEvent(ch.URI)
	}
}

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

type frameWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{w: bufio.NewWriterSize(w, 1<<20)}
}

func (fw *frameWriter) write(body []byte) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	_, _ = fmt.Fprintf(fw.w, "Content-Length: %d\r\n\r\n", len(body))
	_, _ = fw.w.Write(body)
	_ = fw.w.Flush()
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			_, _ = fmt.Sscanf(strings.TrimSpace(v), "%d", &length)
		}
	}
	if length <= 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
