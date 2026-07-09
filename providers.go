package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- shared types ----------

// Part is one renderable chunk of a message.
type Part struct {
	Type string `json:"type"` // text | think | tool | tool_result | image
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"` // tool name
	Data string `json:"data,omitempty"` // tool input / result (stringified)
}

// Message is a single turn in a conversation.
type Message struct {
	ID        string `json:"id"`
	Role      string `json:"role"` // user | assistant | system | tool
	Parts     []Part `json:"parts"`
	Timestamp string `json:"timestamp,omitempty"`
}

// Session is the list-view summary of a conversation.
type Session struct {
	ID           string `json:"id"`       // provider:base64(path)
	Provider     string `json:"provider"` // claude | kimi | gemini | ...
	NativeID     string `json:"native_id"`
	Title        string `json:"title"`
	Project      string `json:"project"`
	Cwd          string `json:"cwd"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	LastPrompt   string `json:"last_prompt"`
	MessageCount int    `json:"message_count"`
	Archived     bool   `json:"archived"`
	updatedUnix  int64  // for sorting, not serialized
}

// Provider is a source of conversations + optional chat capability.
//
// Sémantique de détection, pensée pour une installation quelconque :
//   - Available()   : l'agent est pertinent ici = binaire présent OU sessions sur disque.
//     Un agent fraîchement installé mais jamais lancé (pas de dossier de sessions)
//     doit rester utilisable pour discuter.
//   - HasSessions() : il y a un historique à parcourir.
//   - CanChat()     : on peut lancer une conversation (binaire requis).
type Provider interface {
	Name() string
	Available() bool
	HasSessions() bool
	CanResume() bool
	CanChat() bool
	List() []Session
	Messages(key string) ([]Message, error) // key = decoded path/id
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func encodeID(provider, key string) string {
	return provider + ":" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeID(id string) (provider, key string, ok bool) {
	i := strings.IndexByte(id, ':')
	if i < 0 {
		return "", "", false
	}
	provider = id[:i]
	b, err := base64.RawURLEncoding.DecodeString(id[i+1:])
	if err != nil {
		return "", "", false
	}
	return provider, string(b), true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	// avoid cutting mid-rune
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// small mtime-keyed cache so re-listing hundreds of files is cheap.
type fileCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}
type cacheEntry struct {
	mtime int64
	sess  Session
}

func newFileCache() *fileCache { return &fileCache{m: map[string]cacheEntry{}} }

func (c *fileCache) get(path string, mtime int64) (Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[path]
	if ok && e.mtime == mtime {
		return e.sess, true
	}
	return Session{}, false
}
func (c *fileCache) put(path string, mtime int64, s Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[path] = cacheEntry{mtime: mtime, sess: s}
}

// ============================================================
// Claude Code  ~/.claude/projects/<enc-cwd>/<uuid>.jsonl
// ============================================================

type ClaudeProvider struct {
	root  string
	cache *fileCache
}

func NewClaudeProvider() *ClaudeProvider {
	return &ClaudeProvider{root: filepath.Join(home(), ".claude", "projects"), cache: newFileCache()}
}
func (p *ClaudeProvider) Name() string      { return "claude" }
func (p *ClaudeProvider) Available() bool   { return binExists("claude") || p.HasSessions() }
func (p *ClaudeProvider) HasSessions() bool { return dirExists(p.root) }
func (p *ClaudeProvider) CanResume() bool   { return binExists("claude") }
func (p *ClaudeProvider) CanChat() bool     { return binExists("claude") }

func (p *ClaudeProvider) List() []Session {
	var files []string
	filepath.WalkDir(p.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	return parseConcurrent(files, func(path string, mtime int64) (Session, bool) {
		if s, ok := p.cache.get(path, mtime); ok {
			return s, true
		}
		s, ok := p.summarize(path, mtime)
		if ok {
			p.cache.put(path, mtime, s)
		}
		return s, ok
	})
}

func (p *ClaudeProvider) summarize(path string, mtime int64) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false
	}
	defer f.Close()

	var createdAt, cwd, gitBranch, aiTitle, firstUser, lastPrompt, nativeID string
	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var o map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		var t string
		json.Unmarshal(o["type"], &t)
		switch t {
		case "ai-title":
			json.Unmarshal(o["aiTitle"], &aiTitle)
		case "last-prompt":
			json.Unmarshal(o["lastPrompt"], &lastPrompt)
		case "user", "assistant":
			if createdAt == "" {
				json.Unmarshal(o["timestamp"], &createdAt)
			}
			if cwd == "" {
				json.Unmarshal(o["cwd"], &cwd)
			}
			if gitBranch == "" {
				json.Unmarshal(o["gitBranch"], &gitBranch)
			}
			if nativeID == "" {
				json.Unmarshal(o["sessionId"], &nativeID)
			}
			text := extractText(o["message"])
			if text != "" {
				count++
				if t == "user" && firstUser == "" && !strings.HasPrefix(text, "<") {
					firstUser = text
				}
			}
		}
	}
	if count == 0 && aiTitle == "" {
		return Session{}, false
	}
	if nativeID == "" {
		nativeID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	title := aiTitle
	if title == "" {
		title = truncate(firstUser, 80)
	}
	if title == "" {
		title = "(sans titre)"
	}
	info, _ := os.Stat(path)
	upd := info.ModTime()
	if createdAt == "" {
		createdAt = upd.Format(time.RFC3339)
	}
	project := filepath.Base(cwd)
	if project == "" || project == "." {
		project = filepath.Base(filepath.Dir(path))
	}
	return Session{
		ID:           encodeID("claude", path),
		Provider:     "claude",
		NativeID:     nativeID,
		Title:        title,
		Project:      project,
		Cwd:          cwd,
		CreatedAt:    createdAt,
		UpdatedAt:    upd.Format(time.RFC3339),
		LastPrompt:   truncate(lastPrompt, 160),
		MessageCount: count,
		updatedUnix:  upd.Unix(),
	}, true
}

func (p *ClaudeProvider) Messages(path string) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	i := 0
	for sc.Scan() {
		var o map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		var t string
		json.Unmarshal(o["type"], &t)
		if t != "user" && t != "assistant" {
			continue
		}
		var ts string
		json.Unmarshal(o["timestamp"], &ts)
		role := t
		parts := claudeParts(o["message"])
		if len(parts) == 0 {
			continue
		}
		i++
		out = append(out, Message{ID: itoa(i), Role: role, Parts: parts, Timestamp: ts})
	}
	return out, nil
}

// ============================================================
// Kimi Code  ~/.kimi-code/sessions/wd_*/ses_*/{state.json, agents/main/wire.jsonl}
// ============================================================

type KimiProvider struct {
	root  string
	cache *fileCache
}

func NewKimiProvider() *KimiProvider {
	return &KimiProvider{root: filepath.Join(home(), ".kimi-code", "sessions"), cache: newFileCache()}
}
func (p *KimiProvider) Name() string      { return "kimi" }
func (p *KimiProvider) Available() bool   { return binExists("kimi") || p.HasSessions() }
func (p *KimiProvider) HasSessions() bool { return dirExists(p.root) }
func (p *KimiProvider) CanResume() bool   { return binExists("kimi") }
func (p *KimiProvider) CanChat() bool     { return binExists("kimi") }

func (p *KimiProvider) List() []Session {
	var dirs []string
	wds, _ := os.ReadDir(p.root)
	for _, wd := range wds {
		if !wd.IsDir() {
			continue
		}
		sess, _ := os.ReadDir(filepath.Join(p.root, wd.Name()))
		for _, s := range sess {
			if s.IsDir() && strings.HasPrefix(s.Name(), "ses_") {
				dirs = append(dirs, filepath.Join(p.root, wd.Name(), s.Name()))
			}
		}
	}
	var out []Session
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, d := range dirs {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s, ok := p.summarize(dir); ok {
				mu.Lock()
				out = append(out, s)
				mu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].updatedUnix > out[j].updatedUnix })
	return out
}

type kimiState struct {
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
	Title      string `json:"title"`
	LastPrompt string `json:"lastPrompt"`
	Custom     struct {
		Archived bool `json:"archived"`
	} `json:"custom"`
}

func (p *KimiProvider) summarize(dir string) (Session, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return Session{}, false
	}
	var st kimiState
	if json.Unmarshal(b, &st) != nil {
		return Session{}, false
	}
	wire := filepath.Join(dir, "agents", "main", "wire.jsonl")
	count := countKimiMessages(wire)
	upd := st.UpdatedAt
	var updUnix int64
	if t, err := time.Parse(time.RFC3339, upd); err == nil {
		updUnix = t.Unix()
	} else if info, err := os.Stat(wire); err == nil {
		updUnix = info.ModTime().Unix()
		upd = info.ModTime().Format(time.RFC3339)
	}
	title := st.Title
	if title == "" {
		title = "(sans titre)"
	}
	project := filepath.Base(filepath.Dir(dir)) // wd_<name>_<hash>
	project = strings.TrimPrefix(project, "wd_")
	if i := strings.LastIndexByte(project, '_'); i > 0 {
		project = project[:i]
	}
	native := strings.TrimPrefix(filepath.Base(dir), "ses_")
	return Session{
		ID:           encodeID("kimi", dir),
		Provider:     "kimi",
		NativeID:     native,
		Title:        title,
		Project:      project,
		CreatedAt:    st.CreatedAt,
		UpdatedAt:    upd,
		LastPrompt:   truncate(st.LastPrompt, 160),
		MessageCount: count,
		Archived:     st.Custom.Archived,
		updatedUnix:  updUnix,
	}, true
}

func countKimiMessages(wire string) int {
	f, err := os.Open(wire)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var o struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(sc.Bytes(), &o) == nil && o.Type == "context.append_message" {
			n++
		}
	}
	return n
}

func (p *KimiProvider) Messages(dir string) ([]Message, error) {
	wire := filepath.Join(dir, "agents", "main", "wire.jsonl")
	f, err := os.Open(wire)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	i := 0
	for sc.Scan() {
		var o struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &o) != nil || o.Type != "context.append_message" {
			continue
		}
		parts := kimiParts(o.Message.Content)
		if len(parts) == 0 {
			continue
		}
		i++
		out = append(out, Message{ID: itoa(i), Role: o.Message.Role, Parts: parts})
	}
	return out, nil
}

// ============================================================
// Gemini  ~/.gemini/tmp/<hash>/logs.json  (best effort)
// ============================================================

type GeminiProvider struct{ root string }

func NewGeminiProvider() *GeminiProvider {
	return &GeminiProvider{root: filepath.Join(home(), ".gemini", "tmp")}
}
func (p *GeminiProvider) Name() string      { return "gemini" }
func (p *GeminiProvider) Available() bool   { return binExists("gemini") || p.HasSessions() }
func (p *GeminiProvider) HasSessions() bool { return dirExists(p.root) }
func (p *GeminiProvider) CanResume() bool   { return false }
func (p *GeminiProvider) CanChat() bool     { return binExists("gemini") }

type geminiLogEntry struct {
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message"`
	Timestamp string          `json:"timestamp"`
}

func (p *GeminiProvider) List() []Session {
	dirs, _ := os.ReadDir(p.root)
	var out []Session
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(p.root, d.Name(), "logs.json")
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var entries []geminiLogEntry
		if json.Unmarshal(b, &entries) != nil || len(entries) == 0 {
			continue
		}
		var first, last string
		count := 0
		for _, e := range entries {
			txt := jsonToText(e.Message)
			if txt == "" {
				continue
			}
			count++
			if first == "" {
				first = txt
			}
			last = txt
		}
		if count == 0 {
			continue
		}
		info, _ := os.Stat(path)
		upd := info.ModTime()
		out = append(out, Session{
			ID:           encodeID("gemini", path),
			Provider:     "gemini",
			NativeID:     d.Name(),
			Title:        truncate(first, 80),
			Project:      "gemini",
			CreatedAt:    upd.Format(time.RFC3339),
			UpdatedAt:    upd.Format(time.RFC3339),
			LastPrompt:   truncate(last, 160),
			MessageCount: count,
			updatedUnix:  upd.Unix(),
		})
	}
	return out
}

func (p *GeminiProvider) Messages(path string) ([]Message, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []geminiLogEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	var out []Message
	for i, e := range entries {
		txt := jsonToText(e.Message)
		if txt == "" {
			continue
		}
		role := "user"
		if strings.Contains(strings.ToLower(e.Type), "gemini") || strings.Contains(strings.ToLower(e.Type), "assistant") || strings.Contains(strings.ToLower(e.Type), "model") {
			role = "assistant"
		}
		out = append(out, Message{ID: itoa(i + 1), Role: role, Parts: []Part{{Type: "text", Text: txt}}, Timestamp: e.Timestamp})
	}
	return out, nil
}

// ============================================================
// Grok (xAI CLI)  ~/.grok/sessions/<enc-cwd>/<uuid>/{summary.json, chat_history.jsonl}
// ============================================================

type GrokProvider struct {
	root  string
	cache *fileCache
}

func NewGrokProvider() *GrokProvider {
	return &GrokProvider{root: filepath.Join(home(), ".grok", "sessions"), cache: newFileCache()}
}
func (p *GrokProvider) Name() string      { return "grok" }
func (p *GrokProvider) Available() bool   { return binExists("grok") || p.HasSessions() }
func (p *GrokProvider) HasSessions() bool { return dirExists(p.root) }
func (p *GrokProvider) CanResume() bool   { return binExists("grok") }
func (p *GrokProvider) CanChat() bool     { return binExists("grok") }

func (p *GrokProvider) List() []Session {
	var dirs []string
	cwds, _ := os.ReadDir(p.root)
	for _, cwd := range cwds {
		if !cwd.IsDir() {
			continue
		}
		sess, _ := os.ReadDir(filepath.Join(p.root, cwd.Name()))
		for _, s := range sess {
			if s.IsDir() {
				dirs = append(dirs, filepath.Join(p.root, cwd.Name(), s.Name()))
			}
		}
	}
	var out []Session
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, d := range dirs {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s, ok := p.summarize(dir); ok {
				mu.Lock()
				out = append(out, s)
				mu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].updatedUnix > out[j].updatedUnix })
	return out
}

type grokSummary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	Summary   string `json:"session_summary"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	NumChat   int    `json:"num_chat_messages"`
}

func (p *GrokProvider) summarize(dir string) (Session, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		return Session{}, false
	}
	var st grokSummary
	if json.Unmarshal(b, &st) != nil {
		return Session{}, false
	}
	upd := st.UpdatedAt
	var updUnix int64
	if t, err := time.Parse(time.RFC3339, upd); err == nil {
		updUnix = t.Unix()
	} else if info, err := os.Stat(filepath.Join(dir, "chat_history.jsonl")); err == nil {
		updUnix = info.ModTime().Unix()
		upd = info.ModTime().Format(time.RFC3339)
	}
	title := st.Summary
	if title == "" {
		title = "(sans titre)"
	}
	return Session{
		ID:           encodeID("grok", dir),
		Provider:     "grok",
		NativeID:     st.Info.ID,
		Title:        title,
		Project:      filepath.Base(st.Info.Cwd),
		Cwd:          st.Info.Cwd,
		CreatedAt:    st.CreatedAt,
		UpdatedAt:    upd,
		MessageCount: st.NumChat,
		updatedUnix:  updUnix,
	}, true
}

func (p *GrokProvider) Messages(dir string) ([]Message, error) {
	f, err := os.Open(filepath.Join(dir, "chat_history.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	i := 0
	for sc.Scan() {
		var o map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		var t string
		json.Unmarshal(o["type"], &t)
		parts, role := grokParts(t, o)
		if len(parts) == 0 {
			continue
		}
		i++
		out = append(out, Message{ID: itoa(i), Role: role, Parts: parts})
	}
	return out, nil
}

// ============================================================
// Codex (OpenAI CLI)  ~/.codex/sessions/**/rollout-*.jsonl
// ============================================================
//
// Le format de rollout de codex a bougé selon les versions (ligne à plat, ou
// enveloppée dans "payload"). On parse donc défensivement : on cherche un rôle
// user/assistant puis on extrait le texte quelle que soit la forme du contenu.
// Une ligne non reconnue est ignorée, jamais fatale.

type CodexProvider struct {
	root  string
	cache *fileCache
}

func NewCodexProvider() *CodexProvider {
	return &CodexProvider{root: filepath.Join(home(), ".codex", "sessions"), cache: newFileCache()}
}
func (p *CodexProvider) Name() string      { return "codex" }
func (p *CodexProvider) Available() bool   { return binExists("codex") || p.HasSessions() }
func (p *CodexProvider) HasSessions() bool { return dirExists(p.root) }
func (p *CodexProvider) CanResume() bool   { return false }
func (p *CodexProvider) CanChat() bool     { return binExists("codex") }

func (p *CodexProvider) List() []Session {
	var files []string
	filepath.WalkDir(p.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	return parseConcurrent(files, func(path string, mtime int64) (Session, bool) {
		if s, ok := p.cache.get(path, mtime); ok {
			return s, true
		}
		s, ok := p.summarize(path, mtime)
		if ok {
			p.cache.put(path, mtime, s)
		}
		return s, ok
	})
}

func (p *CodexProvider) summarize(path string, mtime int64) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false
	}
	defer f.Close()
	var firstUser, lastUser string
	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		role, text, ok := codexMessage(sc.Bytes())
		if !ok {
			continue
		}
		count++
		if role == "user" {
			if firstUser == "" {
				firstUser = text
			}
			lastUser = text
		}
	}
	if count == 0 {
		return Session{}, false
	}
	info, _ := os.Stat(path)
	upd := info.ModTime()
	title := truncate(firstUser, 80)
	if title == "" {
		title = "(sans titre)"
	}
	// rollout-<timestamp>-<uuid>.jsonl → on garde l'uuid comme id natif
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	native := base
	if i := strings.LastIndexByte(base, '-'); i >= 0 && len(base)-i > 8 {
		native = base[i+1:]
	}
	return Session{
		ID:           encodeID("codex", path),
		Provider:     "codex",
		NativeID:     native,
		Title:        title,
		Project:      filepath.Base(filepath.Dir(path)),
		CreatedAt:    upd.Format(time.RFC3339),
		UpdatedAt:    upd.Format(time.RFC3339),
		LastPrompt:   truncate(lastUser, 160),
		MessageCount: count,
		updatedUnix:  upd.Unix(),
	}, true
}

func (p *CodexProvider) Messages(path string) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	i := 0
	for sc.Scan() {
		role, text, ok := codexMessage(sc.Bytes())
		if !ok {
			continue
		}
		i++
		out = append(out, Message{ID: itoa(i), Role: role, Parts: []Part{{Type: "text", Text: text}}})
	}
	return out, nil
}

// ============================================================
// Chat-only providers (no browsable local sessions)
// ============================================================

type chatOnlyProvider struct {
	name    string
	canChat func() bool
}

func (p *chatOnlyProvider) Name() string      { return p.name }
func (p *chatOnlyProvider) Available() bool   { return p.canChat() }
func (p *chatOnlyProvider) HasSessions() bool { return false }
func (p *chatOnlyProvider) CanResume() bool   { return false }
func (p *chatOnlyProvider) CanChat() bool     { return p.canChat() }
func (p *chatOnlyProvider) List() []Session   { return nil }
func (p *chatOnlyProvider) Messages(string) ([]Message, error) {
	return nil, nil
}

func NewMistralProvider() Provider {
	return &chatOnlyProvider{name: "mistral", canChat: func() bool { return os.Getenv("MISTRAL_API_KEY") != "" }}
}
func NewQwenProvider() Provider {
	return &chatOnlyProvider{name: "qwen", canChat: func() bool { return binExists("qwen") }}
}
