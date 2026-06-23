package goplslazy

import (
	"encoding/json"
	"strings"
)

type openDoc struct {
	URI        string
	LanguageID string
	Version    int
	Text       string
}

func (p *proxy) trackDidOpen(params json.RawMessage) {
	var dp struct {
		TextDocument struct {
			URI        string `json:"uri"`
			LanguageID string `json:"languageId"`
			Version    int    `json:"version"`
			Text       string `json:"text"`
		} `json:"textDocument"`
	}
	if json.Unmarshal(params, &dp) != nil || dp.TextDocument.URI == "" {
		return
	}
	if dp.TextDocument.LanguageID != "go" && !strings.HasSuffix(uriToPath(dp.TextDocument.URI), ".go") {
		return
	}
	doc := openDoc{
		URI:        dp.TextDocument.URI,
		LanguageID: dp.TextDocument.LanguageID,
		Version:    dp.TextDocument.Version,
		Text:       dp.TextDocument.Text,
	}
	if doc.LanguageID == "" {
		doc.LanguageID = "go"
	}
	p.mu.Lock()
	p.openDocs[doc.URI] = doc
	p.mu.Unlock()
}

func (p *proxy) trackDidChange(params json.RawMessage) {
	var dp struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
		ContentChanges []textDocumentContentChangeEvent `json:"contentChanges"`
	}
	if json.Unmarshal(params, &dp) != nil || dp.TextDocument.URI == "" {
		return
	}
	p.mu.Lock()
	doc, ok := p.openDocs[dp.TextDocument.URI]
	if !ok {
		p.mu.Unlock()
		return
	}
	text, ok := applyContentChanges(doc.Text, dp.ContentChanges)
	if ok {
		doc.Text = text
		doc.Version = dp.TextDocument.Version
		p.openDocs[doc.URI] = doc
	}
	p.mu.Unlock()
}

func (p *proxy) trackDidClose(params json.RawMessage) {
	uri := docURI(params)
	if uri == "" {
		return
	}
	p.mu.Lock()
	delete(p.openDocs, uri)
	p.mu.Unlock()
}

type textDocumentContentChangeEvent struct {
	Range *lspRange `json:"range,omitempty"`
	Text  string    `json:"text"`
}

func applyContentChanges(text string, changes []textDocumentContentChangeEvent) (string, bool) {
	for _, ch := range changes {
		if ch.Range == nil {
			text = ch.Text
			continue
		}
		start, ok := byteOffsetForUTF16Position(text, ch.Range.Start.Line, ch.Range.Start.Character)
		if !ok {
			return text, false
		}
		end, ok := byteOffsetForUTF16Position(text, ch.Range.End.Line, ch.Range.End.Character)
		if !ok || end < start {
			return text, false
		}
		text = text[:start] + ch.Text + text[end:]
	}
	return text, true
}

func byteOffsetForUTF16Position(text string, line, character int) (int, bool) {
	if line < 0 || character < 0 {
		return 0, false
	}
	lineStart := 0
	currentLine := 0
	for i, r := range text {
		if currentLine == line {
			break
		}
		if r == '\n' {
			currentLine++
			lineStart = i + 1
		}
	}
	if currentLine != line {
		return 0, false
	}

	col := 0
	for i, r := range text[lineStart:] {
		if r == '\n' {
			break
		}
		if col == character {
			return lineStart + i, true
		}
		col += utf16CodeUnits(r)
		if col > character {
			return 0, false
		}
	}
	if col == character {
		for i, r := range text[lineStart:] {
			if r == '\n' {
				return lineStart + i, true
			}
		}
		return len(text), true
	}
	return 0, false
}

func utf16CodeUnits(r rune) int {
	if r <= 0xffff {
		return 1
	}
	return 2
}
