package goplslazy

import "testing"

// inlayHintParams is textDocument/inlayHint's request shape; the proxy has
// no need for this method's params elsewhere, so it is defined here rather
// than in a production file.
type inlayHintParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Range        lspRange               `json:"range"`
}

// TestE2ENestedWorktreeModule is the core regression pin for the whole
// multi-worktree-modules feature: a nested Go module (e.g. a git worktree
// with its own go.mod) must work for definition/hover/inlayHint even when
// its file is the very first thing opened in the session — no other file in
// that module, or anywhere else, opened before it. Before this feature this
// scenario failed with "no package metadata" because directoryFilters never
// covered the nested module's path (see plan-worktree-*.md).
func TestE2ENestedWorktreeModule(t *testing.T) {
	skipUnlessE2E(t)

	root, repo, nested := writeMonorepoWithNestedWorktree(t)
	c := startProxy(t, root)
	c.initialize(t, root)

	// Cold-open only the nested module's pkg/b file.
	c.openFile(t, nested.pkgBFile)

	// textDocument/definition is held by the proxy until the requesting
	// file's scope unit is applied (see holdForRequestUnit in proxy.go), so
	// by the time gopls answers, the nested module's view should already be
	// ready — no explicit retry loop needed, matching TestE2E's own
	// definition_first_try budget for a cold open.
	t.Run("nested_definition", func(t *testing.T) {
		resp := c.call(t, "textDocument/definition",
			positionParams(nested.pkgBFile, nested.sumCall, nil),
			e2eBudget(e2eDefinitionBudget))
		got := parseLocationList(t, resp)
		if len(got) != 1 {
			t.Fatalf("want exactly 1 definition location, got %d: %v", len(got), got)
		}
		if got[0].file != nested.pkgAFile || got[0].line != nested.sumDecl.Line {
			t.Fatalf("definition = %s:%d, want %s:%d",
				got[0].file, got[0].line, nested.pkgAFile, nested.sumDecl.Line)
		}
	})

	// hover and inlayHint are not held by the proxy (only definition and
	// prepareRename are). The exact "no package metadata" symptom is a
	// JSON-RPC error response, not merely an empty/null result, so this
	// only asserts the absence of an error.
	t.Run("nested_hover", func(t *testing.T) {
		resp := c.call(t, "textDocument/hover",
			positionParams(nested.pkgBFile, nested.sumCall, nil),
			e2eBudget(e2eDefinitionBudget))
		if len(resp.Error) > 0 {
			t.Fatalf("hover returned a JSON-RPC error (the \"no package metadata\" symptom): %s", resp.Error)
		}
	})

	t.Run("nested_inlay_hint", func(t *testing.T) {
		resp := c.call(t, "textDocument/inlayHint",
			inlayHintParams{
				TextDocument: textDocumentIdentifier{URI: pathToURI(nested.pkgBFile)},
				Range: lspRange{
					Start: lspPosition{Line: 0, Character: 0},
					End:   lspPosition{Line: nested.pkgBEndLine, Character: 0},
				},
			},
			e2eBudget(e2eDefinitionBudget))
		if len(resp.Error) > 0 {
			t.Fatalf("inlayHint returned a JSON-RPC error (the \"no package metadata\" symptom): %s", resp.Error)
		}
	})

	// Opening and querying a nested module must not corrupt the main
	// module's own scope: definition on the main module, in the same
	// session, must behave exactly as TestE2E's definition_first_try.
	c.openFile(t, repo.svc05File)
	t.Run("main_definition_after_nested", func(t *testing.T) {
		resp := c.call(t, "textDocument/definition",
			positionParams(repo.svc05File, repo.sumCall, nil),
			e2eBudget(e2eDefinitionBudget))
		got := parseLocationList(t, resp)
		if len(got) != 1 {
			t.Fatalf("want exactly 1 definition location, got %d: %v", len(got), got)
		}
		if got[0].file != repo.utilFile || got[0].line != repo.sumDecl.Line {
			t.Fatalf("definition = %s:%d, want %s:%d",
				got[0].file, got[0].line, repo.utilFile, repo.sumDecl.Line)
		}
	})
}

// TestE2ENestedWorktreeModule_References pins the per-module reverse-import
// index (revindex.go): references on a nested module's declaration must find
// callers in that module even when the caller's file was never opened —
// mirroring how TestE2E's references_function_cross_package proves the
// closure-widening path works for the main module without pre-opening every
// caller.
func TestE2ENestedWorktreeModule_References(t *testing.T) {
	skipUnlessE2E(t)

	root, _, nested := writeMonorepoWithNestedWorktree(t)
	c := startProxy(t, root)
	c.initialize(t, root)

	// Open only pkg/a; pkg/b is never opened.
	c.openFile(t, nested.pkgAFile)
	resp := c.call(t, methodReferences,
		positionParams(nested.pkgAFile, nested.sumDecl, refContext()),
		e2eBudget(e2eClosureBudget))
	assertLocations(t, resp, nested.pkgBFile)
}

// TestE2ENestedWorktreeModule_CrossModuleOutOfScope pins the scope decision
// in resolve.go's cross-module fallback: a references query rooted in the
// main module must never widen into a nested module's closure, even when a
// same-named symbol exists there. lib/util.Sum and the nested module's own
// Sum are different symbols in different packages/modules; only the former
// must appear.
func TestE2ENestedWorktreeModule_CrossModuleOutOfScope(t *testing.T) {
	skipUnlessE2E(t)

	root, repo, nested := writeMonorepoWithNestedWorktree(t)
	c := startProxy(t, root)
	c.initialize(t, root)

	c.openFile(t, repo.utilFile)
	resp := c.call(t, methodReferences,
		positionParams(repo.utilFile, repo.sumDecl, refContext()),
		e2eBudget(e2eClosureBudget))

	for _, l := range parseLocationList(t, resp) {
		if l.file == nested.pkgBFile {
			t.Fatalf("references on the main module's Sum leaked into the nested module's own (different) Sum call site: %s", l.file)
		}
	}
}
