package goplslazy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const workerTimeout = 2 * time.Minute

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

func (p *proxy) runWorkerRequest(request []byte) (*message, error) {
	state := p.workerState()
	if state.root == "" {
		return nil, fmt.Errorf("workspace root is not initialized")
	}

	cmd := exec.Command(p.opts.gopls, p.opts.goplsArgs...) //nolint:gosec // gopls path comes from user configuration
	cmd.Env = os.Environ()
	if p.opts.driver && p.graph != nil {
		if exe, err := os.Executable(); err == nil {
			cmd.Env = append(cmd.Env,
				"GOPACKAGESDRIVER="+exe,
				"GOPLS_LAZY_DRIVER=1",
				"GOPLS_LAZY_SOCK="+p.graph.sockPath,
			)
		}
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	c := &workerClient{
		state:   state,
		in:      newFrameWriter(stdin),
		out:     bufio.NewReaderSize(stdout, 1<<20),
		pending: map[string]chan workerResponse{},
	}
	go c.readLoop()

	if err := c.initialize(); err != nil {
		return nil, err
	}
	for _, doc := range state.docs {
		c.didOpen(doc)
	}
	return c.request(request)
}

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

type workerClient struct {
	state workerState
	in    *frameWriter
	out   *bufio.Reader

	mu      sync.Mutex
	seq     int
	pending map[string]chan workerResponse
}

func (c *workerClient) initialize() error {
	params := c.state.initParams
	if len(params) == 0 {
		params = c.defaultInitializeParams()
	}
	_, err := c.call("initialize", params, workerTimeout)
	if err != nil {
		return err
	}
	c.notify("initialized", struct{}{})
	return nil
}

func (c *workerClient) defaultInitializeParams() json.RawMessage {
	uri := pathToURI(c.state.root)
	name := filepath.Base(c.state.root)
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
	c.notify("textDocument/didOpen", didOpenParams{
		TextDocument: textDocumentItem(doc),
	})
}

// notify sends a parameterized JSON-RPC notification to the worker gopls.
func (c *workerClient) notify(method string, params any) {
	var raw json.RawMessage
	if b, err := json.Marshal(params); err == nil {
		raw = b
	}
	msg := message{JSONRPC: jsonrpcVersion, Method: method, Params: raw}
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

func (c *workerClient) request(raw []byte) (*message, error) {
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
		return resp.msg, nil
	case <-time.After(workerTimeout):
		c.unregister(string(m.ID))
		return nil, fmt.Errorf("%s timed out", m.Method)
	}
}

func (c *workerClient) call(method string, params json.RawMessage, timeout time.Duration) (*message, error) {
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
		return nil, err
	}
	c.in.write(raw)

	select {
	case resp := <-ch:
		if len(resp.msg.Error) > 0 {
			return resp.msg, fmt.Errorf("%s failed: %s", method, string(resp.msg.Error))
		}
		return resp.msg, nil
	case <-time.After(timeout):
		c.unregister(id)
		return nil, fmt.Errorf("%s timed out", method)
	}
}

func (c *workerClient) readLoop() {
	for {
		raw, err := readFrame(c.out)
		if err != nil {
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

func (c *workerClient) respondToServerRequest(m *message) {
	result := json.RawMessage(`null`)
	if m.Method == "workspace/configuration" {
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

func (c *workerClient) configurationResult(params json.RawMessage) json.RawMessage {
	var cp struct {
		Items []struct {
			Section string `json:"section"`
		} `json:"items"`
	}
	count := len(c.state.config)
	if json.Unmarshal(params, &cp) == nil && len(cp.Items) > 0 {
		count = len(cp.Items)
	}
	items := make([]json.RawMessage, count)
	for i := range items {
		if i < len(c.state.config) && len(c.state.config[i]) > 0 {
			items[i] = c.state.config[i]
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
