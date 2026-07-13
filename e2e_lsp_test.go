package goplslazy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Default budgets for the E2E requests. GOPLS_LAZY_E2E_TIMEOUT, a float
// multiplier such as "2", stretches all of them for slow machines.
const (
	e2eDefinitionBudget = 60 * time.Second // interactive path
	e2eClosureBudget    = 2 * time.Minute  // closure-expanding references
	e2eWorkerBudget     = 5 * time.Minute  // isolated-worker path (cold)
	e2eReuseBudget      = 30 * time.Second // repeated worker query must be warm
)

func e2eBudget(def time.Duration) time.Duration {
	v := os.Getenv("GOPLS_LAZY_E2E_TIMEOUT")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return def
	}
	return time.Duration(float64(def) * f)
}

var (
	e2eBuildOnce sync.Once
	e2eBuildBin  string
	errE2EBuild  error
)

// buildProxyBinary builds ./cmd/gopls-lazy once per test process. The binary
// lands in a process-wide temp dir (not t.TempDir, whose lifetime is tied to
// one test); the OS reclaims it.
func buildProxyBinary(t *testing.T) string {
	t.Helper()
	e2eBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gopls-lazy-e2e") //nolint:usetesting // t.TempDir would delete this once-per-process shared binary mid-suite
		if err != nil {
			errE2EBuild = err
			return
		}
		bin := filepath.Join(dir, "gopls-lazy")
		out, err := exec.Command("go", "build", "-o", bin, "./cmd/gopls-lazy").CombinedOutput() //nolint:gosec // G204: fixed argv building the module's own binary into a test-controlled path
		if err != nil {
			errE2EBuild = fmt.Errorf("go build ./cmd/gopls-lazy: %w\n%s", err, out)
			return
		}
		e2eBuildBin = bin
	})
	if errE2EBuild != nil {
		t.Fatalf("build gopls-lazy: %v", errE2EBuild)
	}
	return e2eBuildBin
}

// lspClient is a minimal stdio LSP client good enough to drive gopls-lazy:
// it correlates responses by id and keeps the server unblocked by answering
// its reverse requests (configuration, progress, capability registration).
type lspClient struct {
	t   *testing.T
	cmd *exec.Cmd
	in  *frameWriter
	out *bufio.Reader

	mu      sync.Mutex
	seq     int
	pending map[string]chan *message
}

// startProxy builds and starts gopls-lazy for the given workspace root with
// an isolated HOME/XDG_CACHE_HOME so proxy scope/graph caches and gopls's own
// caches never leak between runs. On failure the proxy log is dumped.
//
// The proxy runs in its own process group (Setpgid) and the fake home lives
// outside t.TempDir: on teardown the whole group — proxy plus its main and
// worker gopls grandchildren — is killed and reaped before the cache dir is
// removed with errors ignored. gopls shuts down asynchronously and keeps
// writing its on-disk cache until then, so a t.TempDir under it would race
// gopls's exit and fail the test with "directory not empty".
func startProxy(t *testing.T, root string) *lspClient {
	t.Helper()
	bin := buildProxyBinary(t)
	// Deliberately not t.TempDir: its automatic RemoveAll races the still
	// exiting gopls processes. This dir is torn down manually below, after the
	// process group is dead, with errors ignored.
	fakeHome, err := os.MkdirTemp("", "gopls-lazy-e2e-home") //nolint:usetesting // t.TempDir reintroduces the cache-removal race this dir exists to avoid
	if err != nil {
		t.Fatalf("create fake home: %v", err)
	}
	logPath := filepath.Join(fakeHome, "lazy.log")
	stderrPath := filepath.Join(fakeHome, "stderr.log")

	cmd := exec.Command(bin) //nolint:gosec // G204: bin is the module's own binary built into a test-controlled path
	cmd.Dir = root
	cmd.Env = e2eEnv(t, fakeHome, logPath)
	// Own process group so the teardown signal reaches the gopls grandchildren,
	// which inherit it, and not only the proxy PID.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderrFile, err := os.Create(stderrPath) //nolint:gosec // path is inside a test temp dir
	if err != nil {
		t.Fatalf("create stderr log: %v", err)
	}
	cmd.Stderr = stderrFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gopls-lazy: %v", err)
	}
	t.Cleanup(func() {
		// Kill the entire process group (negative pgid == proxy pid, set by
		// Setpgid) so the main and worker gopls processes die too, then wait so
		// none survives to touch fakeHome while it is removed.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
		_ = stderrFile.Close()
		if t.Failed() {
			logFileContent(t, "proxy log", logPath)
			logFileContent(t, "proxy stderr", stderrPath)
		}
		// Ignore errors: a leftover cache file from a gopls still exiting is
		// harmless, whereas t.TempDir's RemoveAll would fail the whole test.
		_ = os.RemoveAll(fakeHome)
	})

	c := &lspClient{
		t:       t,
		cmd:     cmd,
		in:      newFrameWriter(stdin),
		out:     bufio.NewReaderSize(stdout, 1<<20),
		pending: map[string]chan *message{},
	}
	go c.readLoop()
	return c
}

// e2eEnv isolates the child process. HOME/XDG_CACHE_HOME point at a fake
// home; GOPATH and GOMODCACHE are pinned to their real values because the
// fake HOME would otherwise reroute their defaults, and the go toolchain
// gopls shells out to needs the real module cache. Ambient GOPLS_LAZY_*
// configuration is stripped so the test controls the proxy fully.
func e2eEnv(t *testing.T, fakeHome, logPath string) []string {
	t.Helper()
	cache := filepath.Join(fakeHome, ".cache")
	if err := os.MkdirAll(cache, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", cache, err)
	}
	overrides := map[string]string{
		"HOME":           fakeHome,
		"XDG_CACHE_HOME": cache,
		"GOPLS_LAZY_LOG": logPath,
	}
	for _, k := range []string{"GOPATH", "GOMODCACHE"} {
		if v := realGoEnv(k); v != "" {
			overrides[k] = v
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		if strings.HasPrefix(key, "GOPLS_LAZY_") {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

func realGoEnv(key string) string {
	out, err := exec.Command("go", "env", key).Output() //nolint:gosec // G204: fixed argv, key is a constant env-var name from this file
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func logFileContent(t *testing.T, label, path string) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path is inside a test temp dir
	if err != nil || len(b) == 0 {
		return
	}
	t.Logf("%s (%s):\n%s", label, path, b)
}

func (c *lspClient) readLoop() {
	for {
		raw, err := readFrame(c.out)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch {
		case m.Method == "" && m.ID != nil: // response to one of our requests
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		case m.ID != nil: // server->client request
			c.answerServerRequest(&m)
		default: // notification (diagnostics, logs, progress): ignored
		}
	}
}

// answerServerRequest keeps gopls unblocked: workspace/configuration gets one
// empty settings object per requested item (the proxy injects its
// directoryFilters into them); everything else (window/workDoneProgress/
// create, client/registerCapability, ...) gets a null result.
func (c *lspClient) answerServerRequest(m *message) {
	result := json.RawMessage(`null`)
	if m.Method == "workspace/configuration" {
		var cp struct {
			Items []json.RawMessage `json:"items"`
		}
		n := 1
		if json.Unmarshal(m.Params, &cp) == nil && len(cp.Items) > 0 {
			n = len(cp.Items)
		}
		items := make([]json.RawMessage, n)
		for i := range items {
			items[i] = json.RawMessage(`{}`)
		}
		if b, err := json.Marshal(items); err == nil {
			result = b
		}
	}
	resp := message{JSONRPC: jsonrpcVersion, ID: m.ID, Result: result}
	if b, err := json.Marshal(resp); err == nil {
		c.in.write(b)
	}
}

// call sends a request and waits for its response. It fails the test on
// timeout. t is passed explicitly so failures are attributed to the subtest
// issuing the request, not the shared session's parent test.
func (c *lspClient) call(t *testing.T, method string, params any, timeout time.Duration) *message {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", method, err)
	}
	c.mu.Lock()
	c.seq++
	id := strconv.Quote(fmt.Sprintf("e2e-%d", c.seq))
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := message{JSONRPC: jsonrpcVersion, ID: json.RawMessage(id), Method: method, Params: b}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	c.in.write(raw)

	select {
	case m := <-ch:
		return m
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		t.Fatalf("%s timed out after %s", method, timeout)
		return nil
	}
}

func (c *lspClient) notify(t *testing.T, method string, params any) {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", method, err)
	}
	raw, err := json.Marshal(message{JSONRPC: jsonrpcVersion, Method: method, Params: b})
	if err != nil {
		t.Fatalf("marshal %s notification: %v", method, err)
	}
	c.in.write(raw)
}

// initialize performs the LSP handshake, advertising the two capabilities the
// proxy and gopls rely on: workspace.configuration (so directoryFilters flow
// through workspace/configuration) and window.workDoneProgress.
func (c *lspClient) initialize(t *testing.T, root string) {
	t.Helper()
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   pathToURI(root),
		"capabilities": map[string]any{
			"workspace": map[string]any{"configuration": true},
			"window":    map[string]any{"workDoneProgress": true},
		},
		"workspaceFolders": []map[string]any{
			{"uri": pathToURI(root), "name": filepath.Base(root)},
		},
	}
	resp := c.call(t, "initialize", params, e2eBudget(e2eDefinitionBudget))
	if len(resp.Error) > 0 {
		t.Fatalf("initialize failed: %s", resp.Error)
	}
	c.notify(t, "initialized", struct{}{})
}

// openFile sends textDocument/didOpen with the file's on-disk content.
func (c *lspClient) openFile(t *testing.T, path string) {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // path is inside a test temp dir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	c.notify(t, methodDidOpen, didOpenParams{
		TextDocument: textDocumentItem{
			URI:        pathToURI(path),
			LanguageID: "go",
			Version:    1,
			Text:       string(content),
		},
	})
}
