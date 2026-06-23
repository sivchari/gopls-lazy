package goplslazy

import (
	"go/ast"
	"go/token"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

const (
	symbolKindClass     = 5
	symbolKindMethod    = 6
	symbolKindFunction  = 12
	symbolKindVariable  = 13
	symbolKindConstant  = 14
	symbolKindInterface = 11
	symbolKindStruct    = 23
)

const maxWorkspaceSymbols = 500

type indexedSymbol struct {
	Name          string
	Kind          int
	ContainerName string
	RelFile       string
	StartLine     int
	StartChar     int
	EndLine       int
	EndChar       int
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type workspaceSymbol struct {
	Name          string      `json:"name"`
	Kind          int         `json:"kind"`
	Location      lspLocation `json:"location"`
	ContainerName string      `json:"containerName,omitempty"`
}

func collectSymbols(fset *token.FileSet, f *ast.File, rel string) []indexedSymbol {
	if f == nil {
		return nil
	}
	var out []indexedSymbol
	add := func(name *ast.Ident, kind int, container string) {
		if name == nil || name.Name == "_" {
			return
		}
		start := fset.Position(name.Pos())
		end := fset.Position(name.End())
		if !start.IsValid() || !end.IsValid() {
			return
		}
		out = append(out, indexedSymbol{
			Name:          name.Name,
			Kind:          kind,
			ContainerName: container,
			RelFile:       rel,
			StartLine:     start.Line - 1,
			StartChar:     start.Column - 1,
			EndLine:       end.Line - 1,
			EndChar:       end.Column - 1,
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			collectFuncDecl(d, add)
		case *ast.GenDecl:
			collectGenDecl(d, add)
		}
	}
	return out
}

func collectFuncDecl(d *ast.FuncDecl, add func(*ast.Ident, int, string)) {
	if d.Recv != nil && len(d.Recv.List) > 0 {
		add(d.Name, symbolKindMethod, receiverName(d.Recv.List[0].Type))
		return
	}
	add(d.Name, symbolKindFunction, "")
}

func collectGenDecl(d *ast.GenDecl, add func(*ast.Ident, int, string)) {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			add(s.Name, typeSpecKind(s.Type), "")
		case *ast.ValueSpec:
			kind := symbolKindVariable
			if d.Tok == token.CONST {
				kind = symbolKindConstant
			}
			for _, name := range s.Names {
				add(name, kind, "")
			}
		}
	}
}

func typeSpecKind(expr ast.Expr) int {
	switch expr.(type) {
	case *ast.InterfaceType:
		return symbolKindInterface
	case *ast.StructType:
		return symbolKindStruct
	default:
		return symbolKindClass
	}
}

func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.IndexExpr:
		return receiverName(e.X)
	case *ast.IndexListExpr:
		return receiverName(e.X)
	default:
		return ""
	}
}

func (ri *revIndex) WorkspaceSymbols(query string) []workspaceSymbol {
	query = strings.TrimSpace(query)

	ri.mu.RLock()
	root := ri.root
	var matches []scoredSymbol
	for _, symbols := range ri.fileSymbols {
		for i := range symbols {
			if score := symbolMatchScore(query, &symbols[i]); score >= 0 {
				matches = append(matches, scoredSymbol{sym: symbols[i], score: score})
			}
		}
	}
	ri.mu.RUnlock()

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		if matches[i].sym.Name != matches[j].sym.Name {
			return matches[i].sym.Name < matches[j].sym.Name
		}
		return matches[i].sym.RelFile < matches[j].sym.RelFile
	})
	if len(matches) > maxWorkspaceSymbols {
		matches = matches[:maxWorkspaceSymbols]
	}

	out := make([]workspaceSymbol, 0, len(matches))
	for _, match := range matches {
		sym := match.sym
		path := filepath.Join(root, filepath.FromSlash(sym.RelFile))
		out = append(out, workspaceSymbol{
			Name: sym.Name,
			Kind: sym.Kind,
			Location: lspLocation{
				URI: pathToURI(path),
				Range: lspRange{
					Start: lspPosition{Line: sym.StartLine, Character: sym.StartChar},
					End:   lspPosition{Line: sym.EndLine, Character: sym.EndChar},
				},
			},
			ContainerName: sym.ContainerName,
		})
	}
	return out
}

type scoredSymbol struct {
	sym   indexedSymbol
	score int
}

func symbolMatchScore(query string, sym *indexedSymbol) int {
	q := strings.ToLower(query)
	if q == "" {
		return 4
	}
	name := strings.ToLower(sym.Name)
	full := name
	if sym.ContainerName != "" {
		full = strings.ToLower(sym.ContainerName) + "." + name
	}
	switch {
	case name == q || full == q:
		return 0
	case strings.HasPrefix(name, q) || strings.HasPrefix(full, q):
		return 1
	case strings.Contains(name, q) || strings.Contains(full, q):
		return 2
	case fuzzyContains(q, full):
		return 3
	default:
		return -1
	}
}

func fuzzyContains(query, target string) bool {
	if query == "" {
		return false
	}
	i := 0
	for _, r := range target {
		if rune(query[i]) == r {
			i++
			if i == len(query) {
				return true
			}
		}
	}
	return false
}

func pathToURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}
