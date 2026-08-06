package goplslazy

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serveDefinitionAt starts a goroutine that answers every proxy-originated
// pendingOwn request (askDefinition) with a single-element location result
// at defPath, line 0. The caller must call the returned stop func.
func serveDefinitionAt(p *proxy, defPath string) func() {
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			for id, ch := range p.pendingOwn {
				delete(p.pendingOwn, id)
				result, _ := json.Marshal([]map[string]any{{
					"uri": pathToURI(defPath),
					"range": map[string]any{
						"start": map[string]any{"line": 0, "character": 0},
						"end":   map[string]any{"line": 0, "character": 0},
					},
				}})
				ch <- &message{Result: result}
			}
			p.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { close(stop) }
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// TestResolveAndExpand_CrossModuleFallback verifies that when the requesting
// file and the resolved definition belong to different modules,
// resolveAndExpand never computes a reverse-import closure (which would
// require indexes that are intentionally left unset/nil here) and instead
// only widens the requesting file's own scope unit before forwarding.
func TestResolveAndExpand_CrossModuleFallback(t *testing.T) {
	p, buf := newTestProxy()
	p.appliedFallbackDelay = 10 * time.Millisecond
	root := t.TempDir()
	p.root = root
	p.modRoots = &moduleRootCache{}
	// p.idx is intentionally left nil: reaching ClosureUnits/WaitReady on any
	// index (main or nested) would panic here, which is exactly what a
	// regression that computed a cross-module closure would do.

	reqDir := filepath.Join(root, "main", "pkg")
	mustMkdirAll(t, reqDir)
	reqPath := filepath.Join(reqDir, "req.go")
	mustWriteFile(t, reqPath, "package pkg\n")

	modDir := filepath.Join(root, "wt", "nested")
	writeGoMod(t, modDir)
	defDir := filepath.Join(modDir, "sub")
	mustMkdirAll(t, defDir)
	defPath := filepath.Join(defDir, "def.go")
	mustWriteFile(t, defPath, "package sub\n")

	reqUnit, ok := p.unitFor(reqPath)
	if !ok {
		t.Fatal("unitFor(reqPath) not ok")
	}

	stop := serveDefinitionAt(p, defPath)
	defer stop()

	done := make(chan struct{})
	go func() {
		p.resolveAndExpand(methodReferences, "file://"+reqPath, 0, 0,
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"textDocument/references"}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolveAndExpand did not return")
	}

	const nestedClosureUnit = "wt/nested/sub"
	p.mu.Lock()
	_, widened := p.scope[reqUnit]
	_, nestedWidened := p.scope[nestedClosureUnit]
	p.mu.Unlock()
	if !widened {
		t.Errorf("requesting unit %s was not widened into scope", reqUnit)
	}
	if nestedWidened {
		t.Errorf("closure unit %s was added to scope; cross-module fallback must not compute a closure", nestedClosureUnit)
	}
	if !strings.Contains(buf.String(), `"method":"textDocument/references"`) {
		t.Error("held references request not forwarded")
	}
}

// TestResolveAndExpand_SameModuleNestedClosure_PrefixesUnits verifies that
// when the requesting file and the resolved definition belong to the SAME
// nested module, resolveAndExpand computes the closure using that module's
// own index and prefixes every resulting unit with the module's
// root-relative path before publishing it to scope.
func TestResolveAndExpand_SameModuleNestedClosure_PrefixesUnits(t *testing.T) {
	p, _ := newTestProxy()
	p.appliedFallbackDelay = 10 * time.Millisecond
	root := t.TempDir()
	p.root = root
	p.modRoots = &moduleRootCache{}

	modDir := filepath.Join(root, "wt", "nested")
	writeGoMod(t, modDir)

	reqDir := filepath.Join(modDir, "cmd", "reqpkg")
	mustMkdirAll(t, reqDir)
	reqPath := filepath.Join(reqDir, "req.go")
	mustWriteFile(t, reqPath, "package reqpkg\n")

	defDir := filepath.Join(modDir, "pkg", "defpkg")
	mustMkdirAll(t, defDir)
	defPath := filepath.Join(defDir, "def.go")
	mustWriteFile(t, defPath, "package defpkg\n")

	sub := newRevIndex(log.New(io.Discard, "", 0))
	sub.Build(modDir)
	if !sub.Ready() {
		t.Fatal("sub-index not ready after Build")
	}
	p.subIdx = map[string]*revIndex{modDir: sub}

	stop := serveDefinitionAt(p, defPath)
	defer stop()

	done := make(chan struct{})
	go func() {
		p.resolveAndExpand(methodReferences, "file://"+reqPath, 0, 0,
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"textDocument/references"}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolveAndExpand did not return")
	}

	const wantUnit = "wt/nested/pkg/defpkg"
	p.mu.Lock()
	_, gotUnit := p.scope[wantUnit]
	scope := p.scope
	p.mu.Unlock()
	if !gotUnit {
		t.Errorf("expected module-prefixed closure unit %q in scope; scope=%v", wantUnit, scope)
	}
}

// TestServeViaWorker_NestedModuleRoot_FallsBackWithoutTouchingWorker verifies
// that a whole-workspace query whose target is in a nested module never
// calls p.worker (left nil here, so any call would panic) and instead
// widens only the requesting file's own unit.
func TestServeViaWorker_NestedModuleRoot_FallsBackWithoutTouchingWorker(t *testing.T) {
	p, buf := newTestProxy()
	p.appliedFallbackDelay = 10 * time.Millisecond
	root := t.TempDir()
	p.root = root
	p.modRoots = &moduleRootCache{}
	// p.worker is intentionally left nil.

	modDir := filepath.Join(root, "wt", "nested")
	writeGoMod(t, modDir)
	reqDir := filepath.Join(modDir, "pkg")
	mustMkdirAll(t, reqDir)
	reqPath := filepath.Join(reqDir, "req.go")
	mustWriteFile(t, reqPath, "package pkg\n")

	params, err := json.Marshal(struct {
		TextDocument textDocumentIdentifier `json:"textDocument"`
		Position     lspPosition            `json:"position"`
	}{
		TextDocument: textDocumentIdentifier{URI: pathToURI(reqPath)},
		Position:     lspPosition{Line: 0, Character: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := json.Marshal(message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage("1"),
		Method:  methodImplementation,
		Params:  params,
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		p.serveViaWorker(methodImplementation, modDir, held)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveViaWorker did not return")
	}

	if !strings.Contains(buf.String(), `"method":"textDocument/implementation"`) {
		t.Error("held implementation request not forwarded")
	}
}

// TestResolveAndExpand_NoSleepWhenUnitApplied verifies finding 3: when the
// requesting unit is already applied (awaitRequestUnit does not wait) and the
// first askDefinition returns nil, the code must not sleep 2s and re-ask — the
// nil is final. Using a requesting file OUTSIDE the workspace root keeps both
// awaitRequestUnit and the post-resolve path off the reverse-import index.
func TestResolveAndExpand_NoSleepWhenUnitApplied(t *testing.T) {
	p, buf := newTestProxy()
	p.root = t.TempDir()
	const uri = "file:///outside/pkg/x.go"

	var defCount int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			for id, ch := range p.pendingOwn {
				delete(p.pendingOwn, id)
				atomic.AddInt32(&defCount, 1)
				ch <- &message{Result: json.RawMessage("null")}
			}
			p.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	defer close(stop)

	done := make(chan struct{})
	go func() {
		p.resolveAndExpand(methodReferences, uri, 3, 2,
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"textDocument/references"}`))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("resolveAndExpand did not return promptly; the 2s already-applied sleep was not removed")
	}
	if n := atomic.LoadInt32(&defCount); n != 1 {
		t.Errorf("askDefinition called %d times, want 1 (no retry when unit already applied)", n)
	}
	if !strings.Contains(buf.String(), "textDocument/references") {
		t.Error("held references request not forwarded as-is for an out-of-workspace definition")
	}
}

func TestIsMethodDecl(t *testing.T) {
	src := `package x

type T struct{}

func (t *T) Method() {}

func PlainFunc() {}

type I interface {
	IfaceMethod() error
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		line int // 1-based
		want bool
	}{
		{"receiver method", 5, true},
		{"plain func", 7, false},
		{"interface method", 10, true},
		{"struct type line", 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMethodDecl(path, tt.line); got != tt.want {
				t.Errorf("isMethodDecl(line %d) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestMethodNeedsGlobalMethodRefs(t *testing.T) {
	tests := []struct {
		method string
		want   bool
	}{
		{"textDocument/references", true},
		{"textDocument/rename", true},
		{"textDocument/prepareRename", false},
		{"textDocument/implementation", false},
	}
	for _, tt := range tests {
		if got := methodNeedsGlobalMethodRefs(tt.method); got != tt.want {
			t.Errorf("methodNeedsGlobalMethodRefs(%q) = %v, want %v", tt.method, got, tt.want)
		}
	}
}
