// gopls-fleet is an LSP stdio proxy that sits between an editor and gopls
// and dynamically narrows the gopls workspace to the directories the user is
// actually editing, via directoryFilters.
//
// In large single-module monorepos gopls type-checks every workspace package
// in the background after startup. The proxy starts gopls with everything
// excluded ("-**") and widens the scope as files are opened, so memory and
// CPU stay proportional to the dependency cones of the open services instead
// of the whole repository.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	goplsPath := flag.String("gopls", "gopls", "path to the gopls binary")
	granularity := flag.Int("granularity", 3, "number of path segments that form one scope unit (e.g. 3 = go/services/auth)")
	debounce := flag.Duration("debounce", 500*time.Millisecond, "delay before applying a scope change, coalescing bursts of didOpen")
	logPath := flag.String("log", "", "debug log file (default: no logging)")
	flag.Parse()

	logger := log.New(io.Discard, "", log.LstdFlags|log.Lmicroseconds)
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gopls-fleet: open log: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		logger.SetOutput(f)
	}

	cmd := exec.Command(*goplsPath, "serve")
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	serverIn, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-fleet: %v\n", err)
		os.Exit(1)
	}
	serverOut, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-fleet: %v\n", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gopls-fleet: start gopls: %v\n", err)
		os.Exit(1)
	}

	p := &proxy{
		granularity: *granularity,
		debounce:    *debounce,
		scope:       map[string]bool{},
		configIDs:   map[string]bool{},
		toServer:    newFrameWriter(serverIn),
		toClient:    newFrameWriter(os.Stdout),
		log:         logger,
	}

	done := make(chan struct{}, 2)
	go func() { p.pumpClient(bufio.NewReaderSize(os.Stdin, 1<<20)); done <- struct{}{} }()
	go func() { p.pumpServer(bufio.NewReaderSize(serverOut, 1<<20)); done <- struct{}{} }()
	<-done
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

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

	mu        sync.Mutex
	root      string          // workspace root filesystem path
	scope     map[string]bool // scope units relative to root
	configIDs map[string]bool // ids of pending server->client workspace/configuration requests
	timer     *time.Timer

	toServer *frameWriter
	toClient *frameWriter
	log      *log.Logger
}

// pumpClient forwards editor->gopls traffic, patching initialize options,
// configuration responses, and watching didOpen to grow the scope.
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

// pumpServer forwards gopls->editor traffic, remembering the ids of
// workspace/configuration requests so the answers can be patched.
func (p *proxy) pumpServer(r *bufio.Reader) {
	for {
		raw, err := readFrame(r)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw, &m) == nil && m.Method == "workspace/configuration" && m.ID != nil {
			p.mu.Lock()
			p.configIDs[string(m.ID)] = true
			p.mu.Unlock()
		}
		p.toClient.write(raw)
	}
}

func (p *proxy) filters() []string {
	dirs := make([]string, 0, len(p.scope))
	for d := range p.scope {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	fs := []string{"-**"}
	for _, d := range dirs {
		fs = append(fs, "+"+d)
	}
	return fs
}

func (p *proxy) patchInitialize(raw []byte, m *message) []byte {
	var params map[string]any
	if json.Unmarshal(m.Params, &params) != nil {
		return raw
	}
	if uri, _ := params["rootUri"].(string); uri != "" {
		p.mu.Lock()
		p.root = uriToPath(uri)
		p.mu.Unlock()
	} else if folders, _ := params["workspaceFolders"].([]any); len(folders) > 0 {
		if f, _ := folders[0].(map[string]any); f != nil {
			if uri, _ := f["uri"].(string); uri != "" {
				p.mu.Lock()
				p.root = uriToPath(uri)
				p.mu.Unlock()
			}
		}
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
	p.log.Printf("initialize: root=%s filters=%v", p.root, opts["directoryFilters"])
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

func (p *proxy) observeOpen(params json.RawMessage) {
	var dp struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if json.Unmarshal(params, &dp) != nil {
		return
	}
	path := uriToPath(dp.TextDocument.URI)
	if !strings.HasSuffix(path, ".go") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root == "" || !strings.HasPrefix(path, p.root+string(filepath.Separator)) {
		return
	}
	rel, err := filepath.Rel(p.root, filepath.Dir(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	unit := scopeUnit(filepath.ToSlash(rel), p.granularity)
	if p.scope[unit] {
		return
	}
	p.scope[unit] = true
	p.log.Printf("scope += %s (open %s)", unit, rel)
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(p.debounce, p.pushScope)
}

// pushScope tells gopls that configuration changed; gopls then re-requests
// workspace/configuration, and the patched answer carries the new filters.
func (p *proxy) pushScope() {
	note := message{
		JSONRPC: "2.0",
		Method:  "workspace/didChangeConfiguration",
		Params:  json.RawMessage(`{"settings":{}}`),
	}
	raw, err := json.Marshal(note)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.log.Printf("rescope: filters=%v", p.filters())
	p.mu.Unlock()
	p.toServer.write(raw)
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
