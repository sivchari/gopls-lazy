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

	// gopls resolves the definition below against the requesting file's
	// package, so that package must be in an applied view first: right after
	// didOpen the unit can still be pending, and gopls would serve the file
	// in orphan mode and resolve nothing.
	reqPath := uriToPath(uri)
	waited := p.awaitRequestUnit(reqPath)

	// Resolve the defining location; fall back to the requesting file.
	defPath, defLine := reqPath, line
	loc := p.askDefinition(uri, line, char)
	if loc == nil && waited {
		// We blocked for the requesting unit to become applied, so the first ask
		// may have raced the view swap; retry once now that it is applied. When
		// the unit was already applied (waited=false) a nil result is final —
		// retrying would only repeat the same answer, so fall through.
		loc = p.askDefinition(uri, line, char)
	}
	if loc != nil {
		defPath, defLine = loc.path, loc.line
	}
	if rel, err := filepath.Rel(root, filepath.Dir(defPath)); err != nil || strings.HasPrefix(rel, "..") {
		p.log.Printf("crossref %s: definition outside workspace (%s), forwarding as-is", method, defPath)
		p.toServer.write(held)
		return
	}

	// reqModRoot/defModRoot are "" for a file that belongs to the main
	// module, or the nested module's own root (e.g. a git worktree with its
	// own go.mod) otherwise. Cross-module references (including main<->nested)
	// are out of scope by design: only the widened requesting unit is served.
	reqModRoot := p.modRootFor(filepath.Dir(reqPath), root)
	defModRoot := p.modRootFor(filepath.Dir(defPath), root)

	wholeWorkspace := method == methodImplementation ||
		(methodNeedsGlobalMethodRefs(method) && isMethodDecl(defPath, defLine+1))

	if wholeWorkspace {
		// defModRoot (not reqModRoot): the worker must see the module that
		// owns the symbol being queried, which is where defPath lives.
		p.serveViaWorker(method, defModRoot, held)
		return
	}

	if reqModRoot != defModRoot {
		p.log.Printf("crossref %s: cross-module reference (reqModRoot=%q defModRoot=%q); widening only the requesting unit, not a cross-module closure", method, reqModRoot, defModRoot)
		p.widenRequestingUnitAndForward(method, reqPath, held)
		return
	}

	idx := p.indexFor(defModRoot)
	if !idx.WaitReady(120 * time.Second) {
		p.log.Printf("crossref %s: reverse-import index STILL not ready after 120s; forwarding as-is, results may be TRUNCATED", method)
		p.toServer.write(held)
		return
	}

	units, relDir, ok := p.closureUnitsForModule(idx, root, defPath, defModRoot)
	if !ok {
		p.log.Printf("crossref %s: definition outside its module (%s), forwarding as-is", method, defPath)
		p.toServer.write(held)
		return
	}

	p.publishUnitsAndForward(method, units, fmt.Sprintf("closure of %s", relDir), held)
}

// closureUnitsForModule computes idx's reverse-import closure for defPath's
// directory and, for a nested module (defModRoot != ""), prefixes every unit
// with the module's root-relative path so it matches what gopls expects in
// directoryFilters (see unitFor). It reports ok=false when defPath's
// directory cannot be expressed relative to its own module — root for the
// main module, defModRoot for a nested one — which should not happen since
// callers already validated defPath is under root, but is guarded
// defensively like the rest of this file.
func (p *proxy) closureUnitsForModule(idx *revIndex, root, defPath, defModRoot string) (units []string, relDir string, ok bool) {
	base := root
	if defModRoot != "" {
		base = defModRoot
	}
	relDir, err := filepath.Rel(base, filepath.Dir(defPath))
	if err != nil || strings.HasPrefix(relDir, "..") {
		return nil, "", false
	}
	relDir = filepath.ToSlash(relDir)

	units = idx.ClosureUnits(relDir, p.opts.granularity)
	if defModRoot == "" {
		return units, relDir, true
	}
	// defModRoot is guaranteed to be under root by construction of
	// moduleRootCache.RootFor, so this Rel cannot fail.
	prefix, err := filepath.Rel(root, defModRoot)
	if err != nil {
		return nil, "", false
	}
	prefix = filepath.ToSlash(prefix)
	for i, u := range units {
		units[i] = prefixUnit(prefix, u)
	}
	return units, relDir, true
}

// publishUnitsAndForward ensures every unit in units is in scope, waits for
// gopls to apply it, and forwards held. desc names the units for the "held
// request" log line (e.g. "closure of go/pkg/base" or "requesting unit
// go/pkg/base").
//
// Releasing after awaitApplied is a sufficient barrier without a separate
// load-wait: awaitApplied returns only after the patched
// workspace/configuration response reached gopls, and gopls handles stream
// messages in order, so its references/rename handler cannot start until the
// didChangeConfiguration handler has recreated the views; the handler then
// blocks on the new view's metadata load (awaitLoaded) before answering. So
// the forwarded request sees at least the generation-needGen scope. Do not
// add a load-wait here.
func (p *proxy) publishUnitsAndForward(method string, units []string, desc string, held []byte) {
	p.mu.Lock()
	needGen, needPush := p.ensureUnitsLocked(units)
	p.mu.Unlock()
	if needPush {
		needGen = p.pushScope()
		p.log.Printf("crossref %s: %s published at gen %d, holding request", method, desc, needGen)
	}
	if !p.awaitApplied(needGen, 60*time.Second) {
		p.log.Printf("crossref %s: gen %d not applied within 60s; forwarding anyway, results may be TRUNCATED", method, needGen)
	}
	p.toServer.write(held)
}

// widenRequestingUnitAndForward expands the scope to include only reqPath's
// own unit — not a reverse-import closure — and forwards held once that unit
// is applied. Used when a cross-module reference makes a full closure
// computation out of scope (see resolveAndExpand).
func (p *proxy) widenRequestingUnitAndForward(method, reqPath string, held []byte) {
	unit, ok := p.unitFor(reqPath)
	if !ok {
		p.log.Printf("crossref %s: requesting file outside workspace (%s), forwarding as-is", method, reqPath)
		p.toServer.write(held)
		return
	}
	p.publishUnitsAndForward(method, []string{unit}, fmt.Sprintf("requesting unit %s", unit), held)
}

// awaitRequestUnit blocks until the requesting file's scope unit is applied
// in gopls, so a proxy-originated definition request resolves against a real
// view instead of orphan mode. It reports whether it actually waited.
func (p *proxy) awaitRequestUnit(path string) bool {
	unit, ok := p.unitFor(path)
	if !ok {
		return false
	}
	p.mu.Lock()
	e := p.scope[unit]
	gen := 0
	if e != nil {
		gen = e.gen
	}
	applied := gen > 0 && gen <= p.appliedGen
	p.mu.Unlock()
	if e == nil || applied {
		// Never-opened file: proceed as-is (gopls falls back to orphan mode,
		// and the requesting-file fallback below still applies).
		return false
	}
	if !p.awaitUnitApplied(unit, 15*time.Second) {
		p.log.Printf("crossref: requesting unit %s not applied within 15s", unit)
	}
	return true
}

// serveViaWorker answers a cross-reference request from the persistent isolated
// gopls worker that loads the whole workspace, so the long-lived interactive
// gopls is never widened. The first query on a cold worker is slow (the worker
// warms in the background and a showMessage tells the user); subsequent queries
// are fast. Concurrent identical requests (same method and params) are coalesced
// via singleflight so a burst of repeats shares one worker call.
//
// On failure the response depends on the method: rename must not return
// silently-partial results, so it fails with a retryable RequestFailed error;
// references and implementation warn the user and fall back to the narrow main
// gopls scope so the editor still gets whatever results are in view.
//
// modRoot is the module owning the symbol whose implementation/references are
// being queried. modRoot == "" (the main module) uses the persistent
// whole-workspace worker; a nested modRoot (e.g. a git worktree with its own
// go.mod) uses its own per-module worker (workerFor), which loads only that
// module — so it stays warm and cheap regardless of how large the main
// workspace is.
func (p *proxy) serveViaWorker(method, modRoot string, held []byte) {
	h := p.workerFor(modRoot)
	if modRoot == "" {
		p.log.Printf("crossref %s: isolated worker for method or implementation", method)
	} else {
		p.log.Printf("crossref %s: isolated worker for module %s", method, modRoot)
	}
	var req message
	if json.Unmarshal(held, &req) != nil || req.ID == nil {
		p.toServer.write(held)
		return
	}
	if h.shouldWarnLoading() {
		msg := "gopls-lazy: loading the whole workspace for implementation/references; the first query may take minutes"
		if modRoot != "" {
			msg = fmt.Sprintf("gopls-lazy: loading module %s for implementation/references; the first query may take minutes", modRoot)
		}
		p.showMessage(messageInfo, msg)
	}
	// modRoot is folded into the dedup key so an identical-looking request
	// against a different module's worker is never coalesced with this one.
	key := method + "\x00" + modRoot + "\x00" + string(req.Params)
	v, err, shared := p.workerSF.Do(key, func() (any, error) {
		return h.request(held, p.opts.workerTimeout)
	})
	if err != nil {
		p.handleWorkerError(method, req.ID, held, err)
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

// handleWorkerError reports a failed worker query. Rename gets a retryable
// RequestFailed error (a partial rename is dangerous); references and
// implementation warn and fall back to the main gopls scope. prepareRename
// never reaches the worker (it takes the light single-unit path), so it is not
// handled here.
func (p *proxy) handleWorkerError(method string, id, held []byte, err error) {
	switch method {
	case methodRename:
		p.log.Printf("crossref %s: worker unavailable: %v; returning retryable error", method, err)
		p.respondError(id, requestFailedError(
			fmt.Sprintf("gopls-lazy: whole-workspace index unavailable (%v); retry shortly", err)))
	default:
		p.log.Printf("crossref %s: worker unavailable: %v; forwarding to main gopls (results may be incomplete)", method, err)
		p.showMessage(messageWarning, fmt.Sprintf("gopls-lazy: %s results may be incomplete: %v", method, err))
		p.toServer.write(held)
	}
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
		Method:  methodDefinition,
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
