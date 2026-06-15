package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
