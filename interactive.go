package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Mode permission interactif pour Claude : au lieu d'auto-exécuter (bypass), on
// lance claude en flux bidirectionnel (--input-format stream-json). Quand claude
// veut utiliser un outil, il émet un control_request ; on le remonte au front en
// event SSE "ask", et la décision de l'utilisateur revient par POST /permission,
// qu'on réinjecte dans stdin sous forme de control_response.

// runHandle garde le stdin d'un run en cours pour y écrire les réponses de permission.
type runHandle struct {
	stdin io.Writer
	mu    sync.Mutex
}

func (h *runHandle) writeLine(v any) error {
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.stdin.Write(b)
	return err
}

var runs = struct {
	mu sync.Mutex
	m  map[string]*runHandle
}{m: map[string]*runHandle{}}

func registerRun(id string, h *runHandle) {
	runs.mu.Lock()
	runs.m[id] = h
	runs.mu.Unlock()
}
func getRun(id string) *runHandle {
	runs.mu.Lock()
	defer runs.mu.Unlock()
	return runs.m[id]
}
func unregisterRun(id string) {
	runs.mu.Lock()
	delete(runs.m, id)
	runs.mu.Unlock()
}

// claudeBin permet de substituer le binaire claude en test (faux CLI).
func claudeBin() string {
	if b := os.Getenv("LLMWEB_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

func runClaudeInteractive(ctx context.Context, s *sse, cwd, msg, nativeID, model, runID string) {
	bin := claudeBin()
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", "--include-partial-messages",
		"--permission-mode", "default",
	}
	if nativeID != "" {
		args = append(args, "--resume", nativeID)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.send("error", err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.send("error", err.Error())
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		s.send("error", err.Error())
		return
	}
	h := &runHandle{stdin: stdin}
	registerRun(runID, h)
	defer unregisterRun(runID)

	// message utilisateur initial (transport stream-json)
	h.writeLine(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": msg},
	})

	parse := newCLIParser(claudeStreamLine)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	got := false
	for sc.Scan() {
		line := sc.Text()
		if ask, ok := parseControlRequest(line); ok {
			b, _ := json.Marshal(ask)
			s.send("ask", string(b)) // {request_id, tool, input}
			continue
		}
		kind, text, ok := parse(line)
		if !ok || text == "" {
			continue
		}
		if kind == "session" {
			s.send("session", text)
			continue
		}
		got = true
		s.send(kind, text)
	}
	stdin.Close()
	err = cmd.Wait()
	if err != nil && !got {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		s.send("error", msg)
	}
}

// parseControlRequest reconnaît une demande de permission de claude.
func parseControlRequest(line string) (map[string]any, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return nil, false
	}
	var o struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string          `json:"subtype"`
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if json.Unmarshal([]byte(line), &o) != nil || o.Type != "control_request" {
		return nil, false
	}
	if o.Request.Subtype != "can_use_tool" {
		return nil, false
	}
	return map[string]any{
		"request_id": o.RequestID,
		"tool":       o.Request.ToolName,
		"input":      o.Request.Input,
	}, true
}

// handlePermission réinjecte la décision de l'utilisateur dans le run en cours.
func handlePermission(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID     string          `json:"run_id"`
		RequestID string          `json:"request_id"`
		Allow     bool            `json:"allow"`
		Input     json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h := getRun(req.RunID)
	if h == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"ok": false, "error": "run introuvable"})
		return
	}
	var inner map[string]any
	if req.Allow {
		inner = map[string]any{"behavior": "allow"}
		if len(req.Input) > 0 {
			inner["updatedInput"] = req.Input
		}
	} else {
		inner = map[string]any{"behavior": "deny", "message": "Refusé par l'utilisateur"}
	}
	h.writeLine(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": req.RequestID,
			"response":   inner,
		},
	})
	writeJSON(w, map[string]any{"ok": true})
}
