package goplslazy

import (
	"encoding/json"
	"os/exec"
	"sort"
	"testing"
	"time"
)

// skipUnlessE2E skips the end-to-end suite in -short mode and when the real
// gopls or go binaries are not available.
func skipUnlessE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short set; skipping e2e")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not found in PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found in PATH")
	}
}

// TestE2E drives a real gopls through the gopls-lazy proxy over stdio against
// a synthetic 20-service monorepo. The subtests share one proxy session and
// run sequentially; each reproduces one user-visible flow.
func TestE2E(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeMonorepo(t)
	c := startProxy(t, root)
	c.initialize(t, root)
	c.openFile(t, locs.svc05File)

	// Definition must work on the very first try, immediately after didOpen,
	// with no retry hiding a transient failure.
	t.Run("definition_first_try", func(t *testing.T) {
		resp := c.call(t, "textDocument/definition",
			positionParams(locs.svc05File, locs.sumCall, nil),
			e2eBudget(e2eDefinitionBudget))
		got := parseLocationList(t, resp)
		if len(got) != 1 {
			t.Fatalf("want exactly 1 definition location, got %d: %v", len(got), got)
		}
		if got[0].file != locs.utilFile || got[0].line != locs.sumDecl.Line {
			t.Fatalf("definition = %s:%d, want %s:%d",
				got[0].file, got[0].line, locs.utilFile, locs.sumDecl.Line)
		}
	})

	// References on a plain function must cover every importer, including the
	// 19 services outside the current scope (closure-expansion path).
	t.Run("references_function_cross_package", func(t *testing.T) {
		c.openFile(t, locs.utilFile)
		resp := c.call(t, methodReferences,
			positionParams(locs.utilFile, locs.sumDecl, refContext()),
			e2eBudget(e2eClosureBudget))
		assertLocations(t, resp, locs.svcFiles...)
	})

	// References on a concrete method reached only through an interface must
	// include callers that never import the defining package (isolated-worker
	// path; generous budget because the worker loads the whole workspace).
	t.Run("references_interface_method", func(t *testing.T) {
		c.openFile(t, locs.memFile)
		resp := c.call(t, methodReferences,
			positionParams(locs.memFile, locs.memGetDecl, refContext()),
			e2eBudget(e2eWorkerBudget))
		assertLocations(t, resp, locs.svc00File, locs.svc17File)
	})

	// Implementation on the interface method must find the concrete method
	// even though memstore never imports lib/iface (worker path).
	t.Run("implementation", func(t *testing.T) {
		c.openFile(t, locs.ifaceFile)
		resp := c.call(t, methodImplementation,
			positionParams(locs.ifaceFile, locs.storeGetDecl, nil),
			e2eBudget(e2eWorkerBudget))
		assertLocations(t, resp, locs.memFile)
	})

	// Repeating the previous worker query must be fast: a throwaway worker
	// per request re-pays the whole workspace load, a reused/warm worker
	// answers within seconds. 30s is the pass/fail proxy for reuse.
	t.Run("worker_reuse", func(t *testing.T) {
		resp := c.call(t, methodReferences,
			positionParams(locs.memFile, locs.memGetDecl, refContext()),
			e2eBudget(e2eReuseBudget))
		assertLocations(t, resp, locs.svc00File, locs.svc17File)
	})
}

// TestE2EOrphanWindowRequests reproduces the "no package metadata" errors an
// editor gets when it fires requests immediately after didOpen, before the
// rescope that widens gopls's directoryFilters to the new file's unit has
// completed (github issue: vim's blocking "Press ENTER" on every open).
//
// inlayHint and hover errored against real gopls in this window before the
// fix (confirmed empirically); this test pins that they now succeed. The
// parse-only requests must stay instant and unheld, so they are queried the
// same way as a control.
func TestE2EOrphanWindowRequests(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeMonorepo(t)
	c := startProxy(t, root)
	c.initialize(t, root)
	c.openFile(t, locs.svc05File)
	// Let the first unit's rescope complete so opening the next file is a
	// genuine widen into a second, not-yet-applied unit.
	time.Sleep(3 * time.Second)

	// Open a file in a different scope unit and immediately query it, before
	// the rescope that would apply that unit has any chance to complete.
	c.openFile(t, locs.svc17File)

	docParams := textDocumentParams(locs.svc17File)
	inlayParams := textDocumentParams(locs.svc17File)
	inlayParams["range"] = lspRange{Start: lspPosition{Line: 0, Character: 0}, End: lspPosition{Line: 20, Character: 0}}

	t.Run("inlayHint", func(t *testing.T) {
		resp := c.call(t, methodInlayHint, inlayParams, e2eBudget(e2eDefinitionBudget))
		if len(resp.Error) > 0 {
			t.Fatalf("inlayHint errored in the orphan window: %s", resp.Error)
		}
	})
	t.Run("hover", func(t *testing.T) {
		resp := c.call(t, methodHover, positionParams(locs.svc17File, locs.svc17SumCall, nil), e2eBudget(e2eDefinitionBudget))
		if len(resp.Error) > 0 {
			t.Fatalf("hover errored in the orphan window: %s", resp.Error)
		}
		if len(resp.Result) == 0 || string(resp.Result) == "null" {
			t.Fatalf("hover on util.Sum returned no content: %s", resp.Result)
		}
	})

	// Control: parse-only requests must remain unheld and answer instantly
	// even in the same orphan window.
	t.Run("documentSymbol_stays_instant", func(t *testing.T) {
		start := time.Now()
		resp := c.call(t, "textDocument/documentSymbol", docParams, e2eBudget(e2eDefinitionBudget))
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("documentSymbol took %s in the orphan window, want instant (must not be held)", elapsed)
		}
		if len(resp.Error) > 0 {
			t.Fatalf("documentSymbol errored: %s", resp.Error)
		}
	})
	t.Run("foldingRange_stays_instant", func(t *testing.T) {
		start := time.Now()
		resp := c.call(t, "textDocument/foldingRange", docParams, e2eBudget(e2eDefinitionBudget))
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("foldingRange took %s in the orphan window, want instant (must not be held)", elapsed)
		}
		if len(resp.Error) > 0 {
			t.Fatalf("foldingRange errored: %s", resp.Error)
		}
	})
}

// textDocumentParams builds the textDocument-only request params shared by
// document-scoped methods (documentSymbol, foldingRange, inlayHint, ...).
func textDocumentParams(path string) map[string]any {
	return map[string]any{"textDocument": textDocumentIdentifier{URI: pathToURI(path)}}
}

// positionParams builds textDocument/position request params; refCtx adds the
// references-specific context member when non-nil.
func positionParams(path string, pos lspPosition, refCtx map[string]any) map[string]any {
	params := textDocumentParams(path)
	params["position"] = pos
	if refCtx != nil {
		params["context"] = refCtx
	}
	return params
}

func refContext() map[string]any {
	return map[string]any{"includeDeclaration": false}
}

type e2eLocation struct {
	file string
	line int // 0-based
}

// parseLocationList decodes a definition/references/implementation result
// leniently: a Location array, a LocationLink array, a single Location
// object, or null.
func parseLocationList(t *testing.T, resp *message) []e2eLocation {
	t.Helper()
	if resp == nil {
		t.Fatal("nil response")
	}
	if len(resp.Error) > 0 {
		t.Fatalf("request failed: %s", resp.Error)
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil
	}
	type wireLocation struct {
		URI         string   `json:"uri"`
		Range       lspRange `json:"range"`
		TargetURI   string   `json:"targetUri"`
		TargetRange lspRange `json:"targetRange"`
	}
	var wire []wireLocation
	if err := json.Unmarshal(resp.Result, &wire); err != nil {
		var one wireLocation
		if err := json.Unmarshal(resp.Result, &one); err != nil {
			t.Fatalf("cannot parse locations from result: %s", resp.Result)
		}
		wire = []wireLocation{one}
	}
	out := make([]e2eLocation, 0, len(wire))
	for _, w := range wire {
		switch {
		case w.TargetURI != "":
			out = append(out, e2eLocation{file: uriToPath(w.TargetURI), line: w.TargetRange.Start.Line})
		case w.URI != "":
			out = append(out, e2eLocation{file: uriToPath(w.URI), line: w.Range.Start.Line})
		}
	}
	return out
}

// assertLocations fails unless the response contains at least one location in
// every wanted file.
func assertLocations(t *testing.T, resp *message, wantFiles ...string) {
	t.Helper()
	got := parseLocationList(t, resp)
	files := map[string]bool{}
	for _, l := range got {
		files[l.file] = true
	}
	var missing []string
	for _, w := range wantFiles {
		if !files[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		gotFiles := make([]string, 0, len(files))
		for f := range files {
			gotFiles = append(gotFiles, f)
		}
		sort.Strings(gotFiles)
		t.Fatalf("missing locations in %d file(s): %v\ngot %d location(s) across: %v",
			len(missing), missing, len(got), gotFiles)
	}
}
