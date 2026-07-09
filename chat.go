package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type chatRequest struct {
	Provider string `json:"provider"`
	Message  string `json:"message"`
	NativeID string `json:"native_id"` // resume target (optional)
	Cwd      string `json:"cwd"`
	Model    string `json:"model"`
}

// sse is a tiny server-sent-events helper.
type sse struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func newSSE(w http.ResponseWriter) (*sse, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return &sse{w: w, fl: fl}, true
}

func (s *sse) send(kind, text string) {
	b, _ := json.Marshal(map[string]string{"type": kind, "text": text})
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.fl.Flush()
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	s, ok := newSSE(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	cwd := req.Cwd
	if cwd == "" || !dirExists(cwd) {
		cwd = home()
	}

	switch req.Provider {
	case "claude":
		runCLI(ctx, s, cwd, "claude", claudeArgs(req), newCLIParser(claudeStreamLine))
	case "kimi":
		runCLI(ctx, s, cwd, "kimi", kimiArgs(req), newCLIParser(kimiStreamLine))
	case "qwen":
		runCLI(ctx, s, cwd, "qwen", []string{req.Message}, plainLine)
	case "grok":
		runXAI(ctx, s, req)
	case "mistral":
		runMistral(ctx, s, req)
	default:
		s.send("error", "provider inconnu: "+req.Provider)
	}
	s.send("done", "")
}

func claudeArgs(req chatRequest) []string {
	args := []string{}
	if req.NativeID != "" {
		args = append(args, "--resume", req.NativeID)
	}
	// --include-partial-messages : sans lui, claude n'émet un event qu'à la fin
	// de chaque tour et l'UI reste figée jusqu'au bout.
	args = append(args, "-p", req.Message,
		"--output-format", "stream-json", "--verbose", "--include-partial-messages")
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return args
}

func kimiArgs(req chatRequest) []string {
	args := []string{}
	if req.NativeID != "" {
		args = append(args, "-S", req.NativeID)
	}
	args = append(args, "-p", req.Message, "--output-format", "stream-json")
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	return args
}

// lineFn transforms one raw stdout line into text to emit ("" = skip).
// kind is "assistant" (streamed turn), "final" (whole-result fallback) or "token".
type lineFn func(line string) (kind, text string, ok bool)

// newCLIParser wraps a stateless line parser to de-duplicate the common
// "assistant turns + a final result echo" pattern: if we streamed any
// assistant text we drop the final echo; otherwise the final becomes the answer.
func newCLIParser(inner lineFn) lineFn {
	sawDelta, sawAssistant := false, false
	return func(line string) (string, string, bool) {
		kind, text, ok := inner(line)
		if !ok {
			return "", "", false
		}
		switch kind {
		case "delta":
			sawDelta = true
			return "token", text, true
		case "assistant":
			if sawDelta {
				return "", "", false
			}
			sawAssistant = true
			return "token", text, true
		case "final":
			if sawDelta || sawAssistant {
				return "", "", false
			}
			return "token", text, true
		case "tool":
			return "tool", text, true
		default:
			return "token", text, true
		}
	}
}

func runCLI(ctx context.Context, s *sse, cwd, bin string, args []string, transform lineFn) {
	if !binExists(bin) {
		s.send("error", bin+" n'est pas installé")
		return
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
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
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	got := false
	for sc.Scan() {
		if kind, text, ok := transform(sc.Text()); ok && text != "" {
			got = true
			s.send(kind, text)
		}
	}
	err = cmd.Wait()
	if err != nil && !got {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		s.send("error", msg)
	}
}

// claudeStreamLine parses one line of `claude --output-format stream-json`.
func claudeStreamLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return "", "", false
	}
	var o map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &o) != nil {
		return "", "", false
	}
	var t string
	json.Unmarshal(o["type"], &t)
	switch t {
	case "stream_event":
		return claudeStreamEvent(o["event"])
	case "assistant":
		txt := extractText(o["message"])
		if txt != "" {
			return "assistant", txt, true
		}
	case "result":
		var res string
		json.Unmarshal(o["result"], &res)
		if res != "" {
			return "final", res, true
		}
	}
	return "", "", false
}

// claudeStreamEvent lit un event SSE brut de l'API Anthropic remonté par le CLI.
func claudeStreamEvent(raw json.RawMessage) (string, string, bool) {
	if len(raw) == 0 {
		return "", "", false
	}
	var e struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return "", "", false
	}
	switch e.Type {
	case "content_block_delta":
		if e.Delta.Type == "text_delta" && e.Delta.Text != "" {
			return "delta", e.Delta.Text, true
		}
	case "content_block_start":
		if e.ContentBlock.Type == "tool_use" && e.ContentBlock.Name != "" {
			return "tool", e.ContentBlock.Name, true
		}
	}
	return "", "", false
}

// kimiStreamLine parses one line of `kimi --output-format stream-json`.
func kimiStreamLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return "", "", false
	}
	var o map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &o) != nil {
		return "", "", false
	}
	var t string
	json.Unmarshal(o["type"], &t)
	if t == "assistant" || t == "context.append_message" {
		if raw, ok := o["message"]; ok {
			parts := kimiParts(messageContent(raw))
			var sb strings.Builder
			for _, p := range parts {
				if p.Type == "text" {
					sb.WriteString(p.Text)
				}
			}
			if sb.Len() > 0 {
				return "assistant", sb.String(), true
			}
		}
	}
	if raw, ok := o["result"]; ok {
		var res string
		if json.Unmarshal(raw, &res) == nil && res != "" {
			return "final", res, true
		}
	}
	return "", "", false
}

func messageContent(raw json.RawMessage) json.RawMessage {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) == nil && len(m.Content) > 0 {
		return m.Content
	}
	return raw
}

func plainLine(line string) (string, string, bool) {
	if strings.TrimSpace(line) == "" {
		return "", "", false
	}
	return "token", line + "\n", true
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// ---------- OpenAI-compatible API streaming (Grok / Mistral) ----------

func runXAI(ctx context.Context, s *sse, req chatRequest) {
	key := firstEnv("GROK_API_KEY", "XAI_API_KEY")
	if key == "" {
		s.send("error", "GROK_API_KEY non défini")
		return
	}
	model := req.Model
	if model == "" {
		model = "grok-2-latest"
	}
	streamOpenAI(ctx, s, "https://api.x.ai/v1/chat/completions", key, model, req.Message)
}

func runMistral(ctx context.Context, s *sse, req chatRequest) {
	key := os.Getenv("MISTRAL_API_KEY")
	if key == "" {
		s.send("error", "MISTRAL_API_KEY non défini")
		return
	}
	model := req.Model
	if model == "" {
		model = "mistral-large-latest"
	}
	streamOpenAI(ctx, s, "https://api.mistral.ai/v1/chat/completions", key, model, req.Message)
}

func streamOpenAI(ctx context.Context, s *sse, url, key, model, message string) {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": message}},
	})
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		s.send("error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := readAll(resp.Body, 4096)
		s.send("error", fmt.Sprintf("API %d: %s", resp.StatusCode, b))
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				s.send("token", c.Delta.Content)
			}
		}
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func readAll(r interface{ Read([]byte) (int, error) }, max int) (string, error) {
	buf := make([]byte, max)
	n, _ := r.Read(buf)
	return string(buf[:n]), nil
}
