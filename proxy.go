package main

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
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type proxy struct {
	granularity int
	debounce    time.Duration
	useDriver   bool

	mu           sync.Mutex
	root         string          // workspace root filesystem path
	scope        map[string]bool // scope units relative to root
	configIDs    map[string]bool // ids of pending server->client workspace/configuration requests
	pendingDiag  map[string]bool // opened uris whose first diagnostics have not arrived yet
	rescopeTimer *time.Timer
	held         [][]byte // cross-reference requests waiting for the re-scoped view
	awaitingLoad bool     // a held-triggered rescope is in flight
	holdTimer    *time.Timer

	idx   *revIndex
	graph *graphServer

	toServer *frameWriter
	toClient *frameWriter
	log      *log.Logger
}

func (p *proxy) run(goplsPath string) int {
	cmd := exec.Command(goplsPath, "serve")
	cmd.Env = os.Environ()
	if p.useDriver {
		g, err := startGraphServer(p.idx, p.log)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gopls-fleet: graph server: %v (continuing without driver)\n", err)
		} else {
			p.graph = g
			exe, err := os.Executable()
			if err == nil {
				cmd.Env = append(cmd.Env,
					"GOPACKAGESDRIVER="+exe,
					"GOPLS_FLEET_DRIVER=1",
					"GOPLS_FLEET_SOCK="+g.sockPath,
				)
			}
		}
	}
	cmd.Stderr = os.Stderr
	serverIn, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-fleet: %v\n", err)
		return 1
	}
	serverOut, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-fleet: %v\n", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gopls-fleet: start gopls: %v\n", err)
		return 1
	}
	p.toServer = newFrameWriter(serverIn)
	p.toClient = newFrameWriter(os.Stdout)

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
// cross-reference requests until the scope covers their reverse importers.
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
		switch {
		case m.Method == "initialize":
			raw = p.patchInitialize(raw, &m)
		case m.Method == "textDocument/didOpen":
			p.observeOpen(m.Params)
		case m.Method == "textDocument/didSave":
			p.observeFileEvent(docURI(m.Params))
		case m.Method == "workspace/didChangeWatchedFiles":
			p.observeWatchedFiles(m.Params)
		case isCrossRef(m.Method) && m.ID != nil:
			if p.interceptCrossRef(raw, &m) {
				continue // held; forwarded after the re-scope settles
			}
		case m.Method == "" && m.ID != nil:
			// A response from the editor; patch it if it answers a
			// workspace/configuration request we saw going out.
			p.mu.Lock()
			isConfig := p.configIDs[string(m.ID)]
			delete(p.configIDs, string(m.ID))
			p.mu.Unlock()
			if isConfig {
				raw = p.patchConfigResponse(raw, &m)
			}
		}
		p.toServer.write(raw)
	}
}

// pumpServer forwards gopls->editor traffic. It remembers ids of
// workspace/configuration requests, fires the second stage of a two-stage
// rescope once orphan diagnostics arrive, and releases held requests when
// the re-scoped metadata load completes.
func (p *proxy) pumpServer(r *bufio.Reader) {
	for {
		raw, err := readFrame(r)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw, &m) == nil {
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
	case "textDocument/rename", "textDocument/prepareRename",
		"textDocument/references", "textDocument/implementation":
		return true
	}
	return false
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
	}
	opts, _ := params["initializationOptions"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	p.mu.Lock()
	opts["directoryFilters"] = p.filters()
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
	p.mu.Lock()
	fs := p.filters()
	p.mu.Unlock()
	for i, it := range items {
		obj, _ := it.(map[string]any)
		if obj == nil {
			obj = map[string]any{}
		}
		obj["directoryFilters"] = fs
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
	p.log.Printf("configuration response patched: filters=%v", fs)
	return out
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

// observeOpen grows the scope for a newly opened file. The rescope is NOT
// pushed immediately: gopls first serves the file in orphan mode (fast
// diagnostics), and the rescope fires once those diagnostics arrive (or
// after a fallback timeout), so the view reload never delays first feedback.
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
	if p.scope[unit] {
		return
	}
	p.scope[unit] = true
	p.pendingDiag[uri] = true
	p.log.Printf("scope += %s (open, waiting for orphan diagnostics)", unit)
	uriCopy := uri
	time.AfterFunc(3*time.Second, func() { p.diagArrived(uriCopy, "timeout") })
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
	p.rescopeTimer = time.AfterFunc(p.debounce, p.pushScope)
	p.mu.Unlock()
}

// interceptCrossRef expands the scope with the reverse-import closure of the
// request's target package and holds the request until the re-scoped view
// has loaded, so rename/references see every affected package. Returns true
// if the request was held.
func (p *proxy) interceptCrossRef(raw []byte, m *message) bool {
	path := uriToPath(docURI(m.Params))
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	if root == "" || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	rel = filepath.ToSlash(rel)

	if !p.idx.Ready() {
		// Index still building: hold the request in a goroutine and decide
		// once it is ready (cross-reference requests are rare and the editor
		// blocks on them anyway).
		held := append([]byte(nil), raw...)
		go func() {
			if !p.idx.WaitReady(30 * time.Second) {
				p.log.Printf("crossref %s: index not ready in time, forwarding as-is", m.Method)
				p.toServer.write(held)
				return
			}
			if !p.expandForCrossRef(rel, m.Method, held) {
				p.toServer.write(held)
			}
		}()
		return true
	}
	return p.expandForCrossRef(rel, m.Method, raw)
}

// expandForCrossRef returns true if the request was held pending a rescope.
func (p *proxy) expandForCrossRef(relDir, method string, raw []byte) bool {
	units := p.idx.ClosureUnits(relDir, p.granularity)
	p.mu.Lock()
	var need []string
	for _, u := range units {
		if !p.scope[u] {
			need = append(need, u)
		}
	}
	if len(need) == 0 {
		p.mu.Unlock()
		return false
	}
	for _, u := range need {
		p.scope[u] = true
	}
	p.held = append(p.held, append([]byte(nil), raw...))
	p.awaitingLoad = true
	if p.holdTimer != nil {
		p.holdTimer.Stop()
	}
	p.holdTimer = time.AfterFunc(60*time.Second, func() { p.flushHeld("timeout") })
	p.log.Printf("crossref %s in %s: scope += %v, holding request", method, relDir, need)
	p.mu.Unlock()
	p.pushScope()
	return true
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
		return
	}
	go func() {
		changed := p.idx.UpdateFile(path)
		if changed && p.graph != nil {
			p.graph.MarkStale("imports changed: " + path)
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
	fmt.Fprintf(fw.w, "Content-Length: %d\r\n\r\n", len(body))
	fw.w.Write(body)
	fw.w.Flush()
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
			fmt.Sscanf(strings.TrimSpace(v), "%d", &length)
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
