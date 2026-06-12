package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	noEnv := func(string) string { return "" }
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want func(o options) bool
	}{
		{
			name: "defaults",
			args: nil,
			want: func(o options) bool {
				return o.gopls == "gopls" && o.granularity == 3 && o.driver &&
					o.debounce == 500*time.Millisecond && o.evictTTL == 10*time.Minute
			},
		},
		{
			name: "own flags with equals and space",
			args: []string{"-granularity=2", "-evict", "5m", "-driver=false", "-gopls", "/opt/gopls"},
			want: func(o options) bool {
				return o.granularity == 2 && o.evictTTL == 5*time.Minute && !o.driver &&
					o.gopls == "/opt/gopls" && len(o.goplsArgs) == 0
			},
		},
		{
			name: "unknown flags forwarded to gopls",
			args: []string{"-mode=stdio", "-rpc.trace", "-granularity=4"},
			want: func(o options) bool {
				return o.granularity == 4 &&
					reflect.DeepEqual(o.goplsArgs, []string{"-mode=stdio", "-rpc.trace"})
			},
		},
		{
			name: "env defaults",
			env:  map[string]string{"GOPLS_FLEET_GRANULARITY": "2", "GOPLS_FLEET_EVICT": "1m"},
			want: func(o options) bool {
				return o.granularity == 2 && o.evictTTL == time.Minute
			},
		},
		{
			name: "flags override env",
			args: []string{"-granularity=5"},
			env:  map[string]string{"GOPLS_FLEET_GRANULARITY": "2"},
			want: func(o options) bool { return o.granularity == 5 },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := noEnv
			if tt.env != nil {
				getenv = func(k string) string { return tt.env[k] }
			}
			o, err := parseArgs(tt.args, getenv)
			if err != nil {
				t.Fatalf("parseArgs(%v) error: %v", tt.args, err)
			}
			if !tt.want(o) {
				t.Errorf("parseArgs(%v) = %+v", tt.args, o)
			}
		})
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
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
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

func TestParseImports_EmbedSignature(t *testing.T) {
	without := []byte("package x\n\nimport \"embed\"\n\nvar fs embed.FS\n")
	with := []byte("package x\n\nimport \"embed\"\n\n//go:embed assets/*\nvar fs embed.FS\n")
	a1, _ := parseImports(without, "example.com/mod")
	a2, _ := parseImports(with, "example.com/mod")
	if reflect.DeepEqual(a1, a2) {
		t.Error("adding a //go:embed directive should change the file signature")
	}
	a3, _ := parseImports(with, "example.com/mod")
	if !reflect.DeepEqual(a2, a3) {
		t.Error("identical content should produce identical signatures")
	}
}

func TestFiltersLocked_FullWorkspace(t *testing.T) {
	p := &proxy{scope: map[string]*scopeEntry{
		"go/services/auth": {open: map[string]bool{}},
	}}

	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{"-**", "+go/services/auth"}) {
		t.Errorf("scoped filters = %v", got)
	}

	// During a whole-workspace widening the key must be SET to an explicit
	// value (gopls layers configuration over initializationOptions; an
	// absent key would keep the old filters and skip the view reload).
	p.fullUntil = time.Now().Add(time.Minute)
	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{"-**/node_modules"}) {
		t.Errorf("full-workspace filters = %v, want gopls default", got)
	}
	p.userFilters = []string{"-**/testdata"}
	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{"-**/testdata"}) {
		t.Errorf("full-workspace filters = %v, want user filters", got)
	}
}
