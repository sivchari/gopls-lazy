package goplslazy

import (
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
			env:  map[string]string{"GOPLS_LAZY_GRANULARITY": "2", "GOPLS_LAZY_EVICT": "1m"},
			want: func(o options) bool {
				return o.granularity == 2 && o.evictTTL == time.Minute
			},
		},
		{
			name: "flags override env",
			args: []string{"-granularity=5"},
			env:  map[string]string{"GOPLS_LAZY_GRANULARITY": "2"},
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
