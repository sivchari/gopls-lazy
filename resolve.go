package goplslazy

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// interceptCrossRef handles rename/references/implementation requests.
// They are answered correctly only if every package that can mention the
// symbol is in scope, so the proxy:
//
//  1. resolves the symbol's defining location (a definition request to
//     gopls, which works because the requesting file's package and its deps
//     are already loaded),
//  2. decides the required scope: the reverse-import closure of the
//     defining package, or an isolated worker gopls for method symbols
//     (methods can be called through interfaces from packages that never
//     import the defining package) and for implementation requests,
//  3. expands the main gopls scope for closure-safe requests, or runs the
//     request in the worker and returns its response directly.
//
// Returns true if the request was taken over (always, for in-root files).
func (p *proxy) interceptCrossRef(raw []byte, m *message) bool {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"position"`
	}
	if json.Unmarshal(m.Params, &params) != nil {
		return false
	}
	path := uriToPath(params.TextDocument.URI)
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	if root == "" || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return false
	}
	held := append([]byte(nil), raw...)
	method := m.Method
	uri := params.TextDocument.URI
	line, char := params.Position.Line, params.Position.Character
	go func() {
		defer func() {
			if r := recover(); r != nil {
				p.log.Printf("crossref %s: panic: %v; forwarding as-is", method, r)
				p.toServer.write(held)
			}
		}()
		p.resolveAndExpand(method, uri, line, char, held)
	}()
	return true
}

func (p *proxy) resolveAndExpand(method, uri string, line, char int, held []byte) {
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()

	// Resolve the defining location; fall back to the requesting file.
	defPath, defLine := uriToPath(uri), line
	if loc := p.askDefinition(uri, line, char); loc != nil {
		defPath, defLine = loc.path, loc.line
	}
	relDir, err := filepath.Rel(root, filepath.Dir(defPath))
	if err != nil || strings.HasPrefix(relDir, "..") {
		p.log.Printf("crossref %s: definition outside workspace (%s), forwarding as-is", method, defPath)
		p.toServer.write(held)
		return
	}
	relDir = filepath.ToSlash(relDir)

	wholeWorkspace := method == methodImplementation ||
		(methodNeedsGlobalMethodRefs(method) && isMethodDecl(defPath, defLine+1))

	if wholeWorkspace {
		p.serveViaWorker(method, held)
		return
	}

	if !p.idx.WaitReady(30 * time.Second) {
		p.log.Printf("crossref %s: index not ready in time, forwarding as-is", method)
		p.toServer.write(held)
		return
	}

	p.mu.Lock()
	units := p.idx.ClosureUnits(relDir, p.opts.granularity)
	var need []string
	for _, u := range units {
		if p.scope[u] == nil {
			need = append(need, u)
		}
	}
	for _, u := range need {
		p.scope[u] = &scopeEntry{open: map[string]bool{}, lastActive: time.Now()}
	}
	expanded := len(need) > 0
	reason := fmt.Sprintf("closure of %s: +%v", relDir, need)
	if !expanded {
		p.mu.Unlock()
		p.toServer.write(held)
		return
	}
	p.held = append(p.held, held)
	p.awaitingLoad = true
	if p.holdTimer != nil {
		p.holdTimer.Stop()
	}
	p.holdTimer = time.AfterFunc(60*time.Second, func() { p.flushHeld("timeout") })
	p.log.Printf("crossref %s: %s, holding request", method, reason)
	p.mu.Unlock()
	p.pushScope()
}

// serveViaWorker answers a cross-reference request from a short-lived isolated
// gopls worker that loads the whole workspace, so the long-lived interactive
// gopls is never widened. Concurrent identical requests (same method and
// params) are coalesced via singleflight so a burst of repeats spawns a single
// worker instead of one heavy gopls process each.
func (p *proxy) serveViaWorker(method string, held []byte) {
	p.log.Printf("crossref %s: isolated worker for method or implementation", method)
	var req message
	if json.Unmarshal(held, &req) != nil || req.ID == nil {
		p.toServer.write(held)
		return
	}
	key := method + "\x00" + string(req.Params)
	v, err, shared := p.workerSF.Do(key, func() (any, error) {
		return p.runWorkerRequest(held)
	})
	if err != nil {
		p.log.Printf("crossref %s: worker failed: %v; forwarding to current main gopls scope", method, err)
		p.toServer.write(held)
		return
	}
	resp, ok := v.(*message)
	if !ok || resp == nil {
		p.toServer.write(held)
		return
	}
	if shared {
		p.log.Printf("crossref %s: reused in-flight worker result", method)
	}
	if len(resp.Error) > 0 {
		p.respondError(req.ID, resp.Error)
		return
	}
	p.respond(req.ID, resp.Result)
}

func methodNeedsGlobalMethodRefs(method string) bool {
	switch method {
	case methodReferences, methodRename:
		return true
	default:
		return false
	}
}

type defLocation struct {
	path string
	line int // 0-based
}

// askDefinition sends a proxy-originated textDocument/definition request to
// gopls and waits briefly for the answer.
func (p *proxy) askDefinition(uri string, line, char int) *defLocation {
	p.mu.Lock()
	p.ownSeq++
	id := fmt.Sprintf(`"fleet-%d"`, p.ownSeq)
	ch := make(chan *message, 1)
	p.pendingOwn[id] = ch
	p.mu.Unlock()

	params, err := json.Marshal(map[string]any{
		"textDocument": textDocumentIdentifier{URI: uri},
		"position":     lspPosition{Line: line, Character: char},
	})
	if err != nil {
		return nil
	}
	req := message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(id),
		Method:  "textDocument/definition",
		Params:  params,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	p.toServer.write(raw)

	select {
	case m := <-ch:
		return parseDefinition(m.Result)
	case <-time.After(10 * time.Second):
		p.mu.Lock()
		delete(p.pendingOwn, id)
		p.mu.Unlock()
		p.log.Printf("definition resolve timed out for %s", uri)
		return nil
	}
}

func parseDefinition(result json.RawMessage) *defLocation {
	type lspRange struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	}
	type location struct {
		URI         string   `json:"uri"`
		Range       lspRange `json:"range"`
		TargetURI   string   `json:"targetUri"`
		TargetRange lspRange `json:"targetRange"`
	}
	var locs []location
	if err := json.Unmarshal(result, &locs); err != nil {
		var one location
		if json.Unmarshal(result, &one) != nil {
			return nil
		}
		locs = []location{one}
	}
	if len(locs) == 0 {
		return nil
	}
	l := locs[0]
	if l.TargetURI != "" {
		return &defLocation{path: uriToPath(l.TargetURI), line: l.TargetRange.Start.Line}
	}
	if l.URI == "" {
		return nil
	}
	return &defLocation{path: uriToPath(l.URI), line: l.Range.Start.Line}
}

// isMethodDecl reports whether the given 1-based line in the file declares a
// method: a func with a receiver, or a method inside an interface type.
// Purely syntactic; no type information needed.
func isMethodDecl(path string, line int) bool {
	src, err := os.ReadFile(path) //nolint:gosec // path is derived from a LSP location URI provided by gopls
	if err != nil {
		return false
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil || f == nil {
		return false
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found || n == nil {
			return false
		}
		switch d := n.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && fset.Position(d.Name.Pos()).Line == line {
				found = true
				return false
			}
		case *ast.InterfaceType:
			for _, field := range d.Methods.List {
				for _, name := range field.Names {
					if fset.Position(name.Pos()).Line == line {
						found = true
						return false
					}
				}
			}
		}
		return true
	})
	return found
}
