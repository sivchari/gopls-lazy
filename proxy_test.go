package goplslazy

import (
	"encoding/json"
	"io"
	"log"
	"reflect"
	"testing"
)

func TestStripUserFilters(t *testing.T) {
	settings := map[string]any{
		"directoryFilters":       []any{"-**", "+a"},
		"build.directoryFilters": []any{"-**", "+b"},
		"build": map[string]any{
			"directoryFilters": []any{"-**", "+c"},
			"env":              map[string]any{"GOFLAGS": "-tags=x"},
		},
		"ui.semanticTokens": true,
	}
	stripUserFilters(settings)

	for _, k := range []string{"directoryFilters", "build.directoryFilters"} {
		if _, ok := settings[k]; ok {
			t.Errorf("%s should be removed", k)
		}
	}
	build, _ := settings["build"].(map[string]any)
	if _, ok := build["directoryFilters"]; ok {
		t.Error("build.directoryFilters (nested) should be removed")
	}
	if _, ok := build["env"]; !ok {
		t.Error("other nested build settings should be kept")
	}
	if v, _ := settings["ui.semanticTokens"].(bool); !v {
		t.Error("unrelated settings should be kept")
	}
}

// The editor's own directoryFilters (in any spelling gopls accepts) must not
// reach gopls next to the proxy's injected value: gopls uses only the last
// segment of a dotted name, so "build.directoryFilters" is the same option and
// a duplicate makes the applied value depend on map iteration order.
func TestPatchInitialize_StripsUserFilterVariants(t *testing.T) {
	p := &proxy{scope: map[string]*scopeEntry{}, log: log.New(io.Discard, "", 0)}
	params, err := json.Marshal(map[string]any{
		"initializationOptions": map[string]any{
			"build.directoryFilters": []any{"-**", "+go/services/hr"},
			"ui.semanticTokens":      true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &message{JSONRPC: jsonrpcVersion, ID: json.RawMessage("1"), Method: "initialize", Params: params}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	out := p.patchInitialize(raw, m)

	var patched struct {
		Params struct {
			InitializationOptions map[string]any `json:"initializationOptions"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &patched); err != nil {
		t.Fatal(err)
	}
	opts := patched.Params.InitializationOptions
	if _, ok := opts["build.directoryFilters"]; ok {
		t.Error("user's build.directoryFilters should be removed from initialize")
	}
	if got, want := opts["directoryFilters"], []any{"-**"}; !reflect.DeepEqual(got, want) {
		t.Errorf("directoryFilters = %v, want %v (proxy-managed only)", got, want)
	}
	if v, _ := opts["ui.semanticTokens"].(bool); !v {
		t.Error("unrelated initialization options should be kept")
	}

	// The isolated worker must see no directoryFilters at all, so its scope is
	// never truncated by the user's allowlist.
	var workerParams struct {
		InitializationOptions map[string]any `json:"initializationOptions"`
	}
	if err := json.Unmarshal(p.workerInit, &workerParams); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"directoryFilters", "build.directoryFilters"} {
		if _, ok := workerParams.InitializationOptions[k]; ok {
			t.Errorf("worker init params should not contain %s", k)
		}
	}
}

func TestPatchConfigResponse_StripsUserFilterVariants(t *testing.T) {
	p := &proxy{
		scope: map[string]*scopeEntry{"go/services/hr": {open: map[string]bool{}}},
		log:   log.New(io.Discard, "", 0),
	}
	result, err := json.Marshal([]any{map[string]any{
		"build.directoryFilters":    []any{"-**", "+go/services/hr"},
		"ui.diagnostic.staticcheck": false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := &message{JSONRPC: jsonrpcVersion, ID: json.RawMessage("1"), Result: result}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	out := p.patchConfigResponse(raw, m)

	var patched struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(out, &patched); err != nil {
		t.Fatal(err)
	}
	if len(patched.Result) != 1 {
		t.Fatalf("result items = %d, want 1", len(patched.Result))
	}
	item := patched.Result[0]
	if _, ok := item["build.directoryFilters"]; ok {
		t.Error("user's build.directoryFilters should be removed from configuration response")
	}
	if got, want := item["directoryFilters"], []any{"-**", "+go/services/hr"}; !reflect.DeepEqual(got, want) {
		t.Errorf("directoryFilters = %v, want %v (proxy-managed only)", got, want)
	}
	if _, ok := item["ui.diagnostic.staticcheck"]; !ok {
		t.Error("unrelated configuration settings should be kept")
	}

	var workerItem map[string]any
	if err := json.Unmarshal(p.workerConfig[0], &workerItem); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"directoryFilters", "build.directoryFilters"} {
		if _, ok := workerItem[k]; ok {
			t.Errorf("worker config should not contain %s", k)
		}
	}
}
