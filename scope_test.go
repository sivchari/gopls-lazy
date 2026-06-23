package goplslazy

import (
	"reflect"
	"testing"
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

func TestFiltersLocked_RootUnit(t *testing.T) {
	p := &proxy{scope: map[string]*scopeEntry{
		"go/services/auth": {open: map[string]bool{}},
	}}

	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{"-**", "+go/services/auth"}) {
		t.Errorf("scoped filters = %v", got)
	}

	p.scope["."] = &scopeEntry{open: map[string]bool{}}
	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{}) {
		t.Errorf("root-unit filters = %v, want no filters", got)
	}
	p.userFilters = []string{"-**/testdata"}
	if got := p.filtersLocked(); !reflect.DeepEqual(got, []string{"-**/testdata"}) {
		t.Errorf("root-unit filters = %v, want user filters", got)
	}
}
