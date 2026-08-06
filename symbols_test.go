package goplslazy

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
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

func TestMergeWorkspaceSymbols_CombinesAndSorts(t *testing.T) {
	a := []workspaceSymbol{{Name: "Zeta"}, {Name: "Alpha"}}
	b := []workspaceSymbol{{Name: "Mid"}}

	got := mergeWorkspaceSymbols(a, b)

	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := []string{"Alpha", "Mid", "Zeta"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("mergeWorkspaceSymbols names = %v, want %v", names, want)
	}
}

func TestMergeWorkspaceSymbols_RecapsCombinedTotal(t *testing.T) {
	var many []workspaceSymbol
	for i := range maxWorkspaceSymbols + 50 {
		many = append(many, workspaceSymbol{Name: fmt.Sprintf("sym%04d", i)})
	}

	got := mergeWorkspaceSymbols(many, nil)

	if len(got) != maxWorkspaceSymbols {
		t.Fatalf("mergeWorkspaceSymbols len = %d, want cap %d", len(got), maxWorkspaceSymbols)
	}
	// The cap must keep the lexicographically-first results, not an arbitrary
	// prefix of the concatenation.
	if got[0].Name != "sym0000" {
		t.Errorf("first result = %q, want sym0000 (sorted before cap)", got[0].Name)
	}
}
