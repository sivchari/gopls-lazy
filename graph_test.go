package main

import "testing"

func TestGoMinor(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"go1.26.4", 26},
		{"go1.27-devel_69a99fdcbb Sun May 17", 27},
		{"devel +abcdef", 0},
	}
	for _, tt := range tests {
		if got := goMinor(tt.in); got != tt.want {
			t.Errorf("goMinor(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestIsWorkspaceQuery(t *testing.T) {
	if !isWorkspaceQuery([]string{"/repo/...", "builtin"}) {
		t.Error("recursive pattern should be a workspace query")
	}
	if isWorkspaceQuery([]string{"file=/repo/main.go"}) {
		t.Error("file= pattern should not be a workspace query")
	}
}
