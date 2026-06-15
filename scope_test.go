package main

import (
	"reflect"
	"testing"
	"time"
)

func TestScopeUnit(t *testing.T) {
	tests := []struct {
		rel  string
		n    int
		want string
	}{
		{"go/services/auth/internal/tokens", 3, "go/services/auth"},
		{"go/services/auth", 3, "go/services/auth"},
		{"go/pkg", 3, "go/pkg"},
		{"pkg/kubelet/types", 2, "pkg/kubelet"},
		{"main", 3, "main"},
	}
	for _, tt := range tests {
		if got := scopeUnit(tt.rel, tt.n); got != tt.want {
			t.Errorf("scopeUnit(%q, %d) = %q, want %q", tt.rel, tt.n, got, tt.want)
		}
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
