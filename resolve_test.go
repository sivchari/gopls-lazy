package goplslazy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
