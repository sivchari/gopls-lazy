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
	gen        int // scope generation at which the unit was first published to gopls; 0 = not yet published
}

type proxy struct {
	opts options

	mu            sync.Mutex
	root          string                 // workspace root filesystem path
	scope         map[string]*scopeEntry // scope units relative to root
	configIDs     map[string]bool        // ids of pending server->client workspace/configuration requests
	configGens    map[string]int         // config request id -> scopeGen when the pull arrived (lazily allocated)
	sawConfigPull bool                   // true once gopls has issued any workspace/configuration pull (editor supports pull config)
	scopeGen      int                    // monotonically increasing scope generation; bumped by every publish
	appliedGen    int                    // highest generation whose filters have been delivered to gopls
	appliedCh     chan struct{}          // closed and re-made whenever appliedGen advances (lazily allocated)
	heldDefs      map[string][]*heldReq  // document uri -> definition/prepareRename requests held for their unit (lazily allocated)
	workerInit    json.RawMessage        // original initialize params for isolated worker gopls processes
	workerConfig  []json.RawMessage      // original workspace/configuration settings, before lazy filters
	openDocs      map[string]openDoc     // current open document overlays for worker gopls processes
	pendingDiag   map[string]bool        // opened uris whose first diagnostics have not arrived yet
	rescopeTimer  *time.Timer
	pendingOwn    map[string]chan *message // proxy-originated request ids -> response channel
	ownSeq        int

	appliedFallbackDelay time.Duration // test hook; zero means the 10s default
	appliedFallbackCap   time.Duration // test hook; zero means the 60s absolute cap

	workerSF singleflight.Group // dedups concurrent identical isolated-worker requests
	worker   *workerHandle      // persistent isolated gopls for whole-workspace queries

	subWorkersMu sync.Mutex               // guards subWorkers only; kept separate from p.mu, matching subIdxMu
	subWorkers   map[string]*workerHandle // nested module root -> its own isolated worker, built lazily via workerFor

	idx      *revIndex
	graph    *graphServer
	modRoots *moduleRootCache // nested (e.g. worktree) go.mod detection for unitFor; nil is treated as "no nested modules"

	subIdxMu sync.Mutex           // guards subIdx only; kept separate from p.mu, matching the per-concern locking used elsewhere
	subIdx   map[string]*revIndex // nested module root -> its own reverse-import index, built lazily via indexFor

	toServer *frameWriter
	toClient *frameWriter
	log      *log.Logger
}

func (p *proxy) run() int {
	args := p.opts.goplsArgs
	cmd := exec.Command(p.opts.gopls, args...) //nolint:gosec // gopls path comes from user configuration, not untrusted input
	cmd.Env = os.Environ()
	if p.opts.driver {
		g, err := startGraphServer(p.idx, p.modRoots, p.indexFor, p.log)
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
	p.worker.shutdown("proxy exiting")
	p.subWorkersMu.Lock()
	subs := make([]*workerHandle, 0, len(p.subWorkers))
	for _, h := range p.subWorkers {
		subs = append(subs, h)
	}
	p.subWorkersMu.Unlock()
	for _, h := range subs {
		h.shutdown("proxy exiting")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return 0
}

// workerFor returns the isolated worker handle responsible for modRoot: the
// persistent main-workspace worker for "" (the main module), or a lazily
// created, per-module handle for a nested module root (e.g. a git worktree
// with its own go.mod). A second call for the same modRoot returns the same
// handle. The returned handle is not started here — like the main worker, it
// lazily spawns its gopls process on first ensureStarted/acquire/request.
func (p *proxy) workerFor(modRoot string) *workerHandle {
	if modRoot == "" {
		return p.worker
	}
	p.subWorkersMu.Lock()
	defer p.subWorkersMu.Unlock()
	if p.subWorkers == nil {
		p.subWorkers = map[string]*workerHandle{}
	}
	h, ok := p.subWorkers[modRoot]
	if !ok {
		h = &workerHandle{p: p, root: modRoot}
		p.subWorkers[modRoot] = h
	}
	return h
}

// observeWorker mirrors an editor notification onto the isolated worker(s)
// whose view it affects. A document-scoped notification (didOpen/didChange/
// didClose and similar) is delivered only to the worker owning that
// document's module, resolved via workerFor. A workspace-level notification
// with no textDocument (didChangeConfiguration, didChangeWatchedFiles, ...)
// has no single owning module, so it is broadcast to the main worker and
// every ALREADY-CREATED sub-worker — never to a sub-worker that would have to
// be lazily created just to receive it, since that would spin up an idle
// worker for every module ever seen.
func (p *proxy) observeWorker(method string, params json.RawMessage) {
	uri := docURI(params)
	if uri == "" {
		p.broadcastObserve(method, params)
		return
	}
	path := uriToPath(uri)
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	var modRoot string
	if path != "" && root != "" && p.modRoots != nil {
		modRoot = p.modRoots.RootFor(filepath.Dir(path), root)
	}
	p.workerFor(modRoot).observe(method, params)
}

// broadcastObserve delivers a workspace-level notification to the main
// worker and every sub-worker that already exists, without creating any new
// ones.
func (p *proxy) broadcastObserve(method string, params json.RawMessage) {
	p.worker.observe(method, params)
	p.subWorkersMu.Lock()
	subs := make([]*workerHandle, 0, len(p.subWorkers))
	for _, h := range p.subWorkers {
		subs = append(subs, h)
	}
	p.subWorkersMu.Unlock()
	for _, h := range subs {
		h.observe(method, params)
	}
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
		case methodDidChange:
			// A held definition/prepareRename for this document carries the
			// pre-edit cursor position; forward it now, before the didChange
			// reaches gopls, so gopls resolves it against the text it was issued
			// for instead of the shifted lines.
			p.releaseHeldDefs(docURI(m.Params))
			p.trackDidChange(m.Params)
		case "textDocument/didClose":
			p.trackDidClose(m.Params)
			p.observeClose(m.Params)
		case "textDocument/didSave":
			p.observeFileEvent(docURI(m.Params))
		case "workspace/didChangeWatchedFiles":
			p.observeWatchedFiles(m.Params)
		case "":
			if p.handleClientResponse(raw, &m) {
				continue // config response: written to gopls in-order by the handler
			}
		}
		p.observeWorker(m.Method, m.Params)
		p.toServer.write(raw)
	}
}

// interceptClientRequest takes over editor requests the proxy answers itself
// (workspace symbols) or holds until the scope is wide enough. Cross-refs
// (rename/references/implementation) hold for the whole reverse-import closure;
// definition and prepareRename take the light path, holding only until the
// requesting file's own scope unit is applied — prepareRename validates just
// the symbol under the cursor, so it must not pay the closure/worker cost of a
// full rename. It returns true when the request was swallowed and must not be
// forwarded.
func (p *proxy) interceptClientRequest(raw []byte, m *message) bool {
	switch {
	case m.Method == "workspace/symbol":
		return p.interceptWorkspaceSymbol(raw, m)
	case m.Method == methodDefinition:
		return p.interceptDefinition(raw, m)
	case m.Method == methodPrepareRename:
		return p.interceptPrepareRename(raw, m)
	case m.Method == methodInlayHint, m.Method == methodHover:
		// Editors fire these right after didOpen. In gopls's orphan mode (the
		// file's unit not yet applied) both return a JSON-RPC error ("no
		// package metadata for file") instead of degrading gracefully, which
		// editors like vim surface as a blocking error dialog on every open.
		// Confirmed empirically against real gopls; documentSymbol,
		// foldingRange, semanticTokens, codeLens, documentHighlight and
		// signatureHelp all succeed in orphan mode and must stay off this path.
		return p.holdForRequestUnit(raw, m, 15*time.Second)
	case isCrossRef(m.Method):
		return p.interceptCrossRef(raw, m)
	default:
		return false
	}
}

// handleClientResponse forwards an editor->gopls response itself when it
// answers a workspace/configuration pull. The patched response must reach
// gopls strictly before appliedGen advances — a goroutine woken by
// awaitApplied writes its held request immediately, and if that write won
// the race against the config response, gopls would answer it with the old
// view. Writing here and advancing after gives that happens-before edge.
// It returns true when the response was already forwarded.
func (p *proxy) handleClientResponse(raw []byte, m *message) bool {
	if m.ID == nil {
		return false
	}
	p.mu.Lock()
	isConfig := p.configIDs[string(m.ID)]
	gen := p.configGens[string(m.ID)]
	delete(p.configIDs, string(m.ID))
	delete(p.configGens, string(m.ID))
	p.mu.Unlock()
	if !isConfig {
		return false
	}
	p.toServer.write(p.patchConfigResponse(raw, m))
	p.advanceApplied(gen)
	// Workspace-level: every worker's settings changed, not just the main
	// one's, so broadcast rather than routing to a single module's worker.
	p.broadcastObserve("workspace/didChangeConfiguration", nil)
	return true
}

// interceptDefinition holds a definition request until the requesting file's
// scope unit has been applied by gopls. Without this, a definition arriving
// right after didOpen is served from gopls's orphan mode and cross-package
// definitions silently return empty results.
func (p *proxy) interceptDefinition(raw []byte, m *message) bool {
	return p.holdForRequestUnit(raw, m, 15*time.Second)
}

// interceptPrepareRename holds a prepareRename request on the same light
// single-unit path as definition. prepareRename is an interactive validity
// check for the symbol under the cursor: it needs only the requesting file's
// unit loaded, not the reverse-import closure, so it must not go through the
// closure/worker path a full rename uses. The timeout is short because the
// rename popup is blocked until this returns.
func (p *proxy) interceptPrepareRename(raw []byte, m *message) bool {
	return p.holdForRequestUnit(raw, m, 5*time.Second)
}

// holdForRequestUnit holds raw until the requesting file's scope unit has been
// applied by gopls, then forwards it. Used by definition and prepareRename.
//
// The common case (unit already applied) adds zero latency: the request is
// forwarded inline by pumpClient. Held requests are forwarded from a goroutine,
// which can reorder them after later didChange notifications. The request
// carries an absolute cursor position, so a didChange that shifts lines before
// the hold releases would make gopls resolve stale coordinates; the didChange
// path guards against this by releasing any held request for the document
// before the edit reaches gopls (see releaseHeldDefs). A sync.Once makes the
// two possible forwarders (this goroutine and releaseHeldDefs) idempotent.
func (p *proxy) holdForRequestUnit(raw []byte, m *message, timeout time.Duration) bool {
	uri := docURI(m.Params)
	unit, ok := p.unitFor(uriToPath(uri))
	if !ok {
		return false // outside the workspace root
	}
	p.mu.Lock()
	e := p.scope[unit]
	pending := e != nil && (e.gen == 0 || e.gen > p.appliedGen)
	p.mu.Unlock()
	if !pending {
		// Entry nil (request for a never-opened file, e.g. an editor peek
		// feature) or already applied: forward as-is.
		return false
	}
	method := m.Method
	hr := &heldReq{raw: append([]byte(nil), raw...)}
	p.registerHeldDef(uri, hr)
	go func() {
		if !p.awaitUnitApplied(unit, timeout) {
			p.log.Printf("%s: unit %s not applied within %s, forwarding anyway", method, unit, timeout)
		}
		hr.forward(p)
		p.unregisterHeldDef(uri, hr)
	}()
	return true
}

// heldReq is a client request held pending its scope unit. Its forward is
// idempotent so the applied-unit waiter and a racing didChange release can both
// call it; only the first write reaches gopls.
type heldReq struct {
	raw  []byte
	once sync.Once
}

func (h *heldReq) forward(p *proxy) {
	h.once.Do(func() { p.toServer.write(h.raw) })
}

// registerHeldDef records a held definition/prepareRename under its document
// uri so a later didChange for the same document can release it early.
func (p *proxy) registerHeldDef(uri string, hr *heldReq) {
	p.mu.Lock()
	if p.heldDefs == nil {
		p.heldDefs = map[string][]*heldReq{}
	}
	p.heldDefs[uri] = append(p.heldDefs[uri], hr)
	p.mu.Unlock()
}

// unregisterHeldDef drops a held request once its waiter has forwarded it.
func (p *proxy) unregisterHeldDef(uri string, hr *heldReq) {
	p.mu.Lock()
	s := p.heldDefs[uri]
	for i, h := range s {
		if h == hr {
			p.heldDefs[uri] = append(s[:i], s[i+1:]...)
			break
		}
	}
	if len(p.heldDefs[uri]) == 0 {
		delete(p.heldDefs, uri)
	}
	p.mu.Unlock()
}

// releaseHeldDefs forwards every definition/prepareRename still held for uri.
// Called from the didChange path before the edit reaches gopls: the held
// position predates the edit, so forwarding now resolves it against the
// pre-edit text/scope it was issued for rather than the shifted lines.
func (p *proxy) releaseHeldDefs(uri string) {
	p.mu.Lock()
	held := p.heldDefs[uri]
	delete(p.heldDefs, uri)
	p.mu.Unlock()
	for _, hr := range held {
		hr.forward(p)
	}
}

// pumpServer forwards gopls->editor traffic. It remembers ids of
// workspace/configuration requests (and the scope generation current when
// each pull arrived, feeding the applied-generation barrier), routes
// responses to proxy-originated requests, and fires the second stage of a
// two-stage rescope once orphan diagnostics arrive.
func (p *proxy) pumpServer(r *bufio.Reader) {
	for {
		raw, err := readFrame(r)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw, &m) != nil {
			p.toClient.write(raw)
			continue
		}
		if p.handleServerResponse(&m) {
			continue // proxy-originated; not for the editor
		}
		p.handleServerNotification(&m)
		p.toClient.write(raw)
	}
}

// handleServerResponse routes a gopls->proxy response back to the goroutine
// that issued the proxy-originated request (workspace/symbol, cross-refs,
// driver queries) through pendingOwn. It returns true when the response was
// delivered to that goroutine and therefore must not be forwarded to the
// editor.
func (p *proxy) handleServerResponse(m *message) bool {
	if m.Method != "" || m.ID == nil {
		return false
	}
	p.mu.Lock()
	ch := p.pendingOwn[string(m.ID)]
	delete(p.pendingOwn, string(m.ID))
	p.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- m
	return true
}

// handleServerNotification runs the side effects for server->editor traffic
// that is still forwarded verbatim by the caller: it remembers the ids of
// workspace/configuration pulls (with the scope generation live when each
// arrived, feeding the applied-generation barrier) and fires the second stage
// of a two-stage rescope once orphan diagnostics arrive.
func (p *proxy) handleServerNotification(m *message) {
	switch m.Method {
	case methodConfiguration:
		if m.ID == nil {
			return
		}
		p.mu.Lock()
		p.configIDs[string(m.ID)] = true
		// Remember that this editor answers configuration pulls at all, so the
		// applied-generation fallback can tell a merely-slow config-supporting
		// editor apart from one that never pulls (see armAppliedFallback).
		p.sawConfigPull = true
		if p.configGens == nil {
			p.configGens = map[string]int{}
		}
		// The patched response will carry filters covering every unit
		// published up to the current generation: pushScope bumps scopeGen
		// before writing didChangeConfiguration, so a pull arriving here saw
		// that write.
		p.configGens[string(m.ID)] = p.scopeGen
		p.mu.Unlock()
	case "textDocument/publishDiagnostics":
		p.onDiagnostics(m.Params)
	}
}

// isCrossRef reports whether a method needs the whole reverse-import closure
// (or the isolated worker) in scope before it can answer correctly.
// prepareRename is deliberately excluded: it only validates the symbol under
// the cursor and takes the light single-unit path instead.
func isCrossRef(method string) bool {
	switch method {
	case methodRename, methodReferences, methodImplementation:
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
		// A single 10s budget covers the main index and every sub-index below,
		// so N nested modules never turn a 10s budget into N*10s.
		deadline := time.Now().Add(10 * time.Second)
		if !p.idx.WaitReady(time.Until(deadline)) {
			p.log.Printf("workspace/symbol: index not ready in time, forwarding to gopls")
			p.toServer.write(held)
			return
		}
		results := [][]workspaceSymbol{p.idx.WorkspaceSymbols(query)}

		p.subIdxMu.Lock()
		subs := make([]*revIndex, 0, len(p.subIdx))
		for _, ri := range p.subIdx {
			subs = append(subs, ri)
		}
		p.subIdxMu.Unlock()

		for _, ri := range subs {
			remaining := time.Until(deadline)
			if remaining <= 0 || !ri.WaitReady(remaining) {
				// Not ready within the shared budget: skip it rather than block
				// further, so one slow-building sub-index cannot stall the rest.
				continue
			}
			results = append(results, ri.WorkspaceSymbols(query))
		}

		symbols := mergeWorkspaceSymbols(results...)
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

// showMessage sends a window/showMessage notification to the editor, used to
// tell the user that a whole-workspace worker query is loading or degraded.
func (p *proxy) showMessage(typ int, msg string) {
	params, err := json.Marshal(map[string]any{"type": typ, "message": msg})
	if err != nil {
		return
	}
	note := message{JSONRPC: jsonrpcVersion, Method: methodShowMessage, Params: params}
	raw, err := json.Marshal(note)
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
	stripUserFilters(opts)
	workerParams := cloneMap(params)
	workerParams["initializationOptions"] = cloneMap(opts)
	if b, err := json.Marshal(workerParams); err == nil {
		p.mu.Lock()
		p.workerInit = b
		p.mu.Unlock()
	}
	p.mu.Lock()
	// The initial filters (including any restored scope) ride the initialize
	// request itself, which gopls processes before anything else on the
	// stream, so this first generation is applied by construction.
	p.advanceAppliedLocked(p.stampScopeGenLocked())
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
	p.log.Printf("initialize: root=%s filters=%v", root, opts[optionDirectoryFilters])
	if p.opts.workerWarm && root != "" {
		// Start warming the whole-workspace worker now so the first global query
		// does not pay the entire cold load itself.
		go func() { _, _, _, _ = p.worker.ensureStarted() }()
	}
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
			stripUserFilters(obj)
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

// stripUserFilters removes every spelling of directoryFilters from a settings
// object: the flat key, hierarchical dotted names ("build.directoryFilters" —
// gopls uses only the last segment of a dotted name, so it is the same
// option), and the nested "build" section. The proxy owns this option; leaving
// the user's value next to the injected one would make gopls see a duplicate
// and apply whichever the settings-map iteration happens to visit last.
func stripUserFilters(settings map[string]any) {
	for k := range settings {
		if k == optionDirectoryFilters || strings.HasSuffix(k, "."+optionDirectoryFilters) {
			delete(settings, k)
		}
	}
	if build, _ := settings["build"].(map[string]any); build != nil {
		delete(build, optionDirectoryFilters)
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
		p.rescopeTimer = time.AfterFunc(p.opts.debounce, func() { p.pushScope() })
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

// evictLoop drops scope units that have had no open files for the TTL, then
// any nested module whose scope units were just evicted (or never existed)
// and which has no open document under its root.
func (p *proxy) evictLoop() {
	ttl := p.opts.evictTTL
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for range tick.C {
		p.evictIdleUnits(ttl)
		p.evictIdleModules()
	}
}

// evictIdleUnits drops scope units that have had no open files for ttl. Split
// out from evictLoop so tests can drive a sweep directly without waiting on
// the real per-minute ticker.
func (p *proxy) evictIdleUnits(ttl time.Duration) {
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

// evictIdleModules drops the subIdx entry and graph subslot for every nested
// module (e.g. a git worktree with its own go.mod) that currently has no
// surviving scope unit and no open document under its root. It is memory-only
// bookkeeping: a re-touch after eviction lazily rebuilds both via indexFor
// and the graph subslot's own lazy-create-on-miss (subslotFor), exactly as
// for a module never seen before.
//
// The open-docs check is authoritative and independent of the scope-unit
// bookkeeping above: a file can in principle be open without contributing an
// "active" scope entry in edge cases, and evicting a module out from under an
// open file would break active edits, so p.openDocs is consulted directly
// rather than inferred from scope-unit absence.
//
// moduleRootCache entries (p.modRoots) are deliberately left untouched here:
// it caches per QUERIED DIRECTORY, not per module root, so there is no single
// key corresponding to "this module" to drop without an O(n) scan-by-value
// over the whole cache. Its entries never become wrong (go.mod presence
// hasn't changed, only editor activity has), only unnecessary to keep, so
// leaving them is harmless and a scan-and-remove would be over-engineering
// for a cache that self-corrects.
func (p *proxy) evictIdleModules() {
	p.mu.Lock()
	root := p.root
	units := make([]string, 0, len(p.scope))
	for u := range p.scope {
		units = append(units, u)
	}
	docPaths := make([]string, 0, len(p.openDocs))
	for _, doc := range p.openDocs {
		docPaths = append(docPaths, uriToPath(doc.URI))
	}
	p.mu.Unlock()
	if root == "" {
		return
	}

	p.subIdxMu.Lock()
	roots := make(map[string]bool, len(p.subIdx))
	for r := range p.subIdx {
		roots[r] = true
	}
	p.subIdxMu.Unlock()
	if p.graph != nil {
		for _, r := range p.graph.subslotRoots() {
			roots[r] = true
		}
	}

	for modRoot := range roots {
		if docUnderModule(docPaths, modRoot) {
			continue
		}
		prefix := moduleUnitPrefix(root, modRoot)
		if prefix == "" || unitUnderModule(units, prefix) {
			continue
		}
		p.subIdxMu.Lock()
		delete(p.subIdx, modRoot)
		p.subIdxMu.Unlock()
		if p.graph != nil {
			p.graph.RemoveSlot(modRoot)
		}
		p.log.Printf("module -= %s (idle, subIdx+graph subslot evicted)", modRoot)
	}
}

// moduleUnitPrefix returns the root-relative slash path a nested module's own
// scope units are prefixed with (see prefixUnit/unitFor), or "" when modRoot
// is not (strictly) under root.
func moduleUnitPrefix(root, modRoot string) string {
	rel, err := filepath.Rel(root, modRoot)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// unitUnderModule reports whether any scope unit in units belongs to the
// nested module whose scope units are prefixed with prefix (see
// moduleUnitPrefix/prefixUnit).
func unitUnderModule(units []string, prefix string) bool {
	for _, u := range units {
		if u == prefix || strings.HasPrefix(u, prefix+"/") {
			return true
		}
	}
	return false
}

// docUnderModule reports whether any open document path lives at or under
// modRoot. Mirrors the root-prefix check workerStateFor uses to scope
// p.openDocs to a nested module's worker.
func docUnderModule(docPaths []string, modRoot string) bool {
	for _, path := range docPaths {
		if path == modRoot || strings.HasPrefix(path, modRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
	p.rescopeTimer = time.AfterFunc(p.opts.debounce, func() { p.pushScope() })
	p.mu.Unlock()
}

// observeFileEvent keeps the reverse-import index and the package-graph
// cache in sync with on-disk changes (saves, git operations seen by the
// editor's file watcher).
// invalidateModuleBoundary drops caches that go stale when a go.mod at path
// appears or disappears: the module-root cache (every cached RootFor answer
// may now be wrong) and the subIdx entry keyed by this go.mod's own
// directory (the module root it defines). The subIdx entry is stale either
// way — dropping it here makes it lazily rebuild on next access via
// indexFor, whether the module just appeared (no entry yet, a harmless
// no-op delete) or just disappeared (the entry indexed a module that no
// longer exists).
func (p *proxy) invalidateModuleBoundary(gomodPath string) {
	if p.modRoots != nil {
		p.modRoots.Invalidate()
	}
	modDir := filepath.Dir(gomodPath)
	p.subIdxMu.Lock()
	delete(p.subIdx, modDir)
	p.subIdxMu.Unlock()
}

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
	if p.handleModuleFileEvent(path, base) {
		return
	}
	if !strings.HasSuffix(path, ".go") {
		// A non-Go file affects the package graph only when it is an //go:embed
		// asset. Invalidating on every non-Go change (build output, generated
		// JSON, editor temp files) would fire a full `go list ./...` rebuild on
		// noise and starve type-checking, so check the embed footprint first.
		if p.graph != nil && p.graph.IsEmbedFile(path) {
			p.graph.MarkStaleFor(path, "embedded asset changed: "+base)
		}
		return
	}
	go p.updateFileInIndex(path, root)
}

// handleModuleFileEvent handles a save/watch event for a module-defining
// file (go.mod, go.sum, go.work, go.work.sum), invalidating the caches that
// depend on it. It reports whether path was such a file (and was therefore
// handled and requires no further processing by observeFileEvent).
func (p *proxy) handleModuleFileEvent(path, base string) bool {
	if base != "go.mod" && base != "go.sum" && base != "go.work" && base != "go.work.sum" {
		return false
	}
	if base == "go.mod" {
		p.invalidateModuleBoundary(path)
	}
	if p.graph != nil {
		p.graph.MarkStaleFor(path, "module file changed: "+base)
	}
	return true
}

// updateFileInIndex re-parses a changed .go file in the reverse-import index
// that owns its module (see indexFor) and marks the package graph stale if
// its import or embed footprint changed. Run in a goroutine by
// observeFileEvent.
func (p *proxy) updateFileInIndex(path, root string) {
	var modRoot string
	if p.modRoots != nil {
		modRoot = p.modRoots.RootFor(filepath.Dir(path), root)
	}
	changed := p.indexFor(modRoot).UpdateFile(path)
	if changed && p.graph != nil {
		p.graph.MarkStaleFor(path, "imports or embed directives changed: "+path)
	}
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
