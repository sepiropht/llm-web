package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
)

func itoa(i int) string { return strconv.Itoa(i) }

var binCache sync.Map

func binExists(name string) bool {
	if v, ok := binCache.Load(name); ok {
		return v.(bool)
	}
	_, err := exec.LookPath(name)
	ok := err == nil
	binCache.Store(name, ok)
	return ok
}

// parseConcurrent runs fn over files with a bounded worker pool and returns
// sessions sorted newest-first.
func parseConcurrent(files []string, fn func(path string, mtime int64) (Session, bool)) []Session {
	var out []Session
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 24)
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		mtime := info.ModTime().Unix()
		wg.Add(1)
		go func(path string, mt int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s, ok := fn(path, mt); ok {
				mu.Lock()
				out = append(out, s)
				mu.Unlock()
			}
		}(f, mtime)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].updatedUnix > out[j].updatedUnix })
	return out
}

// extractText pulls a flat text preview out of a claude/anthropic message blob
// whose content is either a string or an array of content blocks.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	// string content
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	// array of blocks
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		var t string
		json.Unmarshal(b["type"], &t)
		if t == "text" {
			var txt string
			json.Unmarshal(b["text"], &txt)
			sb.WriteString(txt)
			sb.WriteByte(' ')
		}
	}
	return strings.TrimSpace(sb.String())
}

// claudeParts converts a claude message blob into renderable parts.
func claudeParts(raw json.RawMessage) []Part {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []Part{{Type: "text", Text: s}}
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	var parts []Part
	for _, b := range blocks {
		var t string
		json.Unmarshal(b["type"], &t)
		switch t {
		case "text":
			var txt string
			json.Unmarshal(b["text"], &txt)
			if strings.TrimSpace(txt) != "" {
				parts = append(parts, Part{Type: "text", Text: txt})
			}
		case "thinking":
			var txt string
			json.Unmarshal(b["thinking"], &txt)
			if strings.TrimSpace(txt) != "" {
				parts = append(parts, Part{Type: "think", Text: txt})
			}
		case "tool_use":
			var name string
			json.Unmarshal(b["name"], &name)
			parts = append(parts, Part{Type: "tool", Name: name, Data: rawToString(b["input"])})
		case "tool_result":
			parts = append(parts, Part{Type: "tool_result", Data: contentToString(b["content"])})
		case "image":
			parts = append(parts, Part{Type: "image", Text: "[image]"})
		}
	}
	return parts
}

// kimiParts converts a kimi wire content array into renderable parts.
func kimiParts(raw json.RawMessage) []Part {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []Part{{Type: "text", Text: s}}
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var parts []Part
	for _, b := range blocks {
		var t string
		json.Unmarshal(b["type"], &t)
		switch t {
		case "text":
			var txt string
			json.Unmarshal(b["text"], &txt)
			if strings.TrimSpace(txt) != "" {
				parts = append(parts, Part{Type: "text", Text: txt})
			}
		case "think":
			var txt string
			json.Unmarshal(b["think"], &txt)
			if strings.TrimSpace(txt) != "" {
				parts = append(parts, Part{Type: "think", Text: txt})
			}
		case "tool_use", "tool_call":
			var name string
			json.Unmarshal(b["name"], &name)
			parts = append(parts, Part{Type: "tool", Name: name, Data: rawToString(b["input"])})
		case "image", "image_url":
			parts = append(parts, Part{Type: "image", Text: "[image]"})
		}
	}
	return parts
}

// grokParts converts one grok chat_history.jsonl entry into renderable parts.
func grokParts(t string, o map[string]json.RawMessage) ([]Part, string) {
	switch t {
	case "user":
		txt := grokUserText(o["content"])
		if txt == "" {
			return nil, ""
		}
		return []Part{{Type: "text", Text: txt}}, "user"
	case "reasoning":
		txt := grokReasoning(o["summary"])
		if txt == "" {
			return nil, ""
		}
		return []Part{{Type: "think", Text: txt}}, "assistant"
	case "assistant":
		var parts []Part
		var content string
		json.Unmarshal(o["content"], &content)
		if strings.TrimSpace(content) != "" {
			parts = append(parts, Part{Type: "text", Text: content})
		}
		var calls []struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		json.Unmarshal(o["tool_calls"], &calls)
		for _, c := range calls {
			parts = append(parts, Part{Type: "tool", Name: c.Name, Data: c.Arguments})
		}
		if len(parts) == 0 {
			return nil, ""
		}
		return parts, "assistant"
	case "tool_result":
		data := contentToString(o["content"])
		if data == "" {
			return nil, ""
		}
		return []Part{{Type: "tool_result", Data: data}}, "assistant"
	}
	return nil, ""
}

func extractGrokContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" || b.Text != "" {
				sb.WriteString(b.Text)
				sb.WriteByte(' ')
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// grokUserText nettoie les blocs de contexte injectés par grok (<user_info>,
// <git_status>, …) pour ne garder que le vrai message humain.
func grokUserText(raw json.RawMessage) string {
	txt := extractGrokContent(raw)
	// le vrai prompt est dans <user_query> quand il est présent
	if i := strings.Index(txt, "<user_query>"); i >= 0 {
		rest := txt[i+len("<user_query>"):]
		if j := strings.Index(rest, "</user_query>"); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
		return strings.TrimSpace(rest)
	}
	// sinon : un vrai message humain ne commence pas par une balise. Si après
	// trim le texte commence par "<", c'est un bundle de contexte injecté
	// (<rules>, <git_status>, <agent_skills>…) → on le saute.
	txt = strings.TrimSpace(txt)
	if strings.HasPrefix(txt, "<") {
		return ""
	}
	return txt
}

func grokReasoning(raw json.RawMessage) string {
	var blocks []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	var sb strings.Builder
	for _, b := range blocks {
		if b.Text != "" {
			sb.WriteString(b.Text)
			sb.WriteByte('\n')
		}
	}
	return strings.TrimSpace(sb.String())
}

// codexMessage extrait (rôle, texte) d'une ligne de rollout codex, à plat ou
// enveloppée dans "payload". Renvoie ok=false pour tout ce qui n'est pas un
// tour user/assistant porteur de texte.
func codexMessage(raw []byte) (string, string, bool) {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil {
		return "", "", false
	}
	if p, has := o["payload"]; has {
		var inner map[string]json.RawMessage
		if json.Unmarshal(p, &inner) == nil && len(inner) > 0 {
			o = inner
		}
	}
	var role string
	json.Unmarshal(o["role"], &role)
	if role != "user" && role != "assistant" {
		return "", "", false
	}
	text := strings.TrimSpace(jsonToText(o["content"]))
	if text == "" {
		return "", "", false
	}
	return role, text, true
}

func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// contentToString flattens a tool_result content (string or block array).
func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			var txt string
			json.Unmarshal(b["text"], &txt)
			sb.WriteString(txt)
			sb.WriteByte('\n')
		}
		return strings.TrimSpace(sb.String())
	}
	return string(raw)
}

// jsonToText best-effort extracts human text from an arbitrary gemini message blob.
func jsonToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"text", "content", "message", "parts"} {
			if v, ok := m[k]; ok {
				if t := jsonToText(v); t != "" {
					return t
				}
			}
		}
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var sb strings.Builder
		for _, e := range arr {
			sb.WriteString(jsonToText(e))
			sb.WriteByte(' ')
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}
