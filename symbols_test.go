package goplslazy

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceSymbols(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/mod\n\ngo 1.26\n")
	write("go/services/accounting/journal.go", `package accounting

const journalKind = "entry"

var defaultLimit = 100

type JournalEntry struct{}

type Store interface {
	Get() (JournalEntry, error)
}

func NewJournalEntry() JournalEntry { return JournalEntry{} }

func (j *JournalEntry) Get() (JournalEntry, error) { return JournalEntry{}, nil }
`)

	ri := newRevIndex(log.New(io.Discard, "", 0))
	ri.Build(root)

	got := ri.WorkspaceSymbols("NewJournalEntry")
	if len(got) != 1 {
		t.Fatalf("WorkspaceSymbols(NewJournalEntry) returned %d results, want 1: %#v", len(got), got)
	}
	if got[0].Name != "NewJournalEntry" || got[0].Kind != symbolKindFunction {
		t.Fatalf("WorkspaceSymbols(NewJournalEntry)[0] = %#v", got[0])
	}
	if !strings.HasPrefix(got[0].Location.URI, "file://") {
		t.Fatalf("symbol URI = %q, want file URI", got[0].Location.URI)
	}

	got = ri.WorkspaceSymbols("journalentry.get")
	if len(got) != 1 {
		t.Fatalf("WorkspaceSymbols(journalentry.get) returned %d results, want 1: %#v", len(got), got)
	}
	if got[0].Name != "Get" || got[0].Kind != symbolKindMethod || got[0].ContainerName != "JournalEntry" {
		t.Fatalf("method symbol = %#v", got[0])
	}

	if got := ri.WorkspaceSymbols(""); len(got) == 0 {
		t.Fatal("WorkspaceSymbols(empty) returned no results")
	}
}

func TestParseFileMetadataSymbols(t *testing.T) {
	src := []byte(`package x

type T struct{}
type I interface{ M() }
const C = 1
var V = 2
func F() {}
func (t *T) M() {}
`)
	_, _, symbols := parseFileMetadata(src, "example.com/mod", "x.go")
	want := map[string]int{
		"T": symbolKindStruct,
		"I": symbolKindInterface,
		"C": symbolKindConstant,
		"V": symbolKindVariable,
		"F": symbolKindFunction,
		"M": symbolKindMethod,
	}
	if len(symbols) != len(want) {
		t.Fatalf("symbols = %#v, want %d symbols", symbols, len(want))
	}
	for _, sym := range symbols {
		if kind, ok := want[sym.Name]; !ok || sym.Kind != kind {
			t.Fatalf("unexpected symbol %#v", sym)
		}
	}
}
