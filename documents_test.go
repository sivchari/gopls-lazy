package goplslazy

import "testing"

func TestApplyContentChangesFullText(t *testing.T) {
	got, ok := applyContentChanges("hello", []textDocumentContentChangeEvent{{Text: "world"}})
	if !ok {
		t.Fatal("applyContentChanges failed")
	}
	if got != "world" {
		t.Fatalf("text = %q, want world", got)
	}
}

func TestApplyContentChangesIncrementalUTF16(t *testing.T) {
	text := "package main\n\nvar s = \"🙂\"\n"
	got, ok := applyContentChanges(text, []textDocumentContentChangeEvent{
		{
			Range: &lspRange{
				Start: lspPosition{Line: 2, Character: 8},
				End:   lspPosition{Line: 2, Character: 12},
			},
			Text: "\"ok\"",
		},
	})
	if !ok {
		t.Fatal("applyContentChanges failed")
	}
	want := "package main\n\nvar s = \"ok\"\n"
	if got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}
