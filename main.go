package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

var providers []Provider
var authToken string
var trustPrivate bool
var bypassPermissions bool

// CGNAT : plage utilisée par les VPN maillés (netbird, tailscale).
var cgnat = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

func main() {
	port := flag.Int("port", 8090, "bind port")
	host := flag.String("host", "", "bind host; empty = 127.0.0.1 (this machine only), pass a value to expose")
	token := flag.String("token", "", "auth token (generated if empty)")
	noAuth := flag.Bool("no-auth", false, "skip the token for private/VPN clients (loopback, RFC1918, CGNAT 100.64/10); public clients still need it")
	bypass := flag.Bool("bypass-permissions", false, "laisse les LLM exécuter les outils sans confirmation (--dangerously-skip-permissions / --yolo)")
	flag.Parse()

	trustPrivate = *noAuth
	bypassPermissions = *bypass

	if *token != "" {
		authToken = *token
	} else if env := os.Getenv("LLMWEB_TOKEN"); env != "" {
		authToken = env
	} else {
		authToken = randToken()
	}

	providers = []Provider{
		NewClaudeProvider(),
		NewKimiProvider(),
		NewGeminiProvider(),
		NewGrokProvider(),
		NewMistralProvider(),
		NewQwenProvider(),
	}

	mux := http.NewServeMux()

	// ---- API ----
	mux.HandleFunc("/api/v1/providers", auth(handleProviders))
	mux.HandleFunc("/api/v1/config", auth(handleConfig))
	mux.HandleFunc("/api/v1/sessions", auth(handleSessions))
	mux.HandleFunc("/api/v1/sessions/", auth(handleSessionSub))
	mux.HandleFunc("/api/v1/chat", auth(handleChat))

	// ---- static SPA (unauthenticated; contains no data) ----
	sub, _ := fs.Sub(webFS, "web")
	indexHTML, _ := fs.ReadFile(sub, "index.html")
	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || p == "index.html" {
			serveIndex(w)
			return
		}
		if _, err := fs.Stat(sub, p); err != nil {
			serveIndex(w) // SPA fallback
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	bindHost := *host
	display := bindHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
		display = "127.0.0.1"
	} else if bindHost == "0.0.0.0" || bindHost == "true" {
		bindHost = "0.0.0.0"
		display = localIP()
	}
	addr := fmt.Sprintf("%s:%d", bindHost, *port)

	banner(display, *port)

	srv := &http.Server{Addr: addr, Handler: logMW(mux)}
	log.Fatal(srv.ListenAndServe())
}

func banner(host string, port int) {
	url := fmt.Sprintf("http://%s:%d/#token=%s", host, port, authToken)
	fmt.Printf("\n  \033[38;5;213m▐█▛█▛█▌\033[0m  LLM Web — toutes tes sessions, tous tes LLM\n")
	fmt.Printf("  \033[38;5;213m▐█████▌\033[0m  port %d\n\n", port)
	names := []string{}
	for _, p := range providers {
		if p.Available() {
			names = append(names, p.Name())
		}
	}
	fmt.Printf("  Providers: %s\n", strings.Join(names, ", "))
	fmt.Printf("  URL:       %s\n", url)
	fmt.Printf("  Token:     %s\n\n", authToken)
}

// ---------- auth ----------

// isTrustedAddr : le client est-il sur un réseau privé ou un VPN maillé ?
// On lit uniquement RemoteAddr : X-Forwarded-For est forgeable par le client
// et l'accepter rendrait le contournement trivial.
func isTrustedAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if trustPrivate && isTrustedAddr(r.RemoteAddr) {
			h(w, r)
			return
		}
		tok := ""
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			tok = strings.TrimPrefix(a, "Bearer ")
		}
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(authToken)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(envelope(401, "unauthorized", nil))
			return
		}
		h(w, r)
	}
}

// ---------- API handlers ----------

func envelope(code int, msg string, data any) map[string]any {
	return map[string]any{"code": code, "msg": msg, "data": data, "request_id": randToken()[:16]}
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(envelope(0, "success", data))
}

type providerInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	CanChat   bool   `json:"can_chat"`
	CanResume bool   `json:"can_resume"`
	HasSess   bool   `json:"has_sessions"`
}

func handleProviders(w http.ResponseWriter, r *http.Request) {
	items := []providerInfo{}
	for _, p := range providers {
		_, hasSess := p.(interface{ List() []Session })
		hs := hasSess
		// chat-only providers technically implement List but return nil; flag by name
		switch p.Name() {
		case "grok", "mistral", "qwen":
			hs = false
		}
		items = append(items, providerInfo{
			Name:      p.Name(),
			Available: p.Available(),
			CanChat:   p.CanChat(),
			CanResume: p.CanResume(),
			HasSess:   hs,
		})
	}
	writeJSON(w, map[string]any{"items": items})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"name":    "LLM Web",
		"version": "1.0.0",
	})
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	prov := r.URL.Query().Get("provider")
	includeArchived := r.URL.Query().Get("archived") == "1"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	var all []Session
	ch := make(chan []Session, len(providers))
	n := 0
	for _, p := range providers {
		if !p.Available() {
			continue
		}
		if prov != "" && p.Name() != prov {
			continue
		}
		n++
		go func(pr Provider) { ch <- pr.List() }(p)
	}
	for i := 0; i < n; i++ {
		all = append(all, (<-ch)...)
	}

	filtered := all[:0]
	for _, s := range all {
		if s.Archived && !includeArchived {
			continue
		}
		if q != "" && !matchSession(s, q) {
			continue
		}
		filtered = append(filtered, s)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].updatedUnix > filtered[j].updatedUnix })
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	writeJSON(w, map[string]any{"items": filtered, "total": len(filtered)})
}

func matchSession(s Session, q string) bool {
	return strings.Contains(strings.ToLower(s.Title), q) ||
		strings.Contains(strings.ToLower(s.LastPrompt), q) ||
		strings.Contains(strings.ToLower(s.Project), q) ||
		strings.Contains(strings.ToLower(s.Provider), q)
}

// /api/v1/sessions/{id}/messages
func handleSessionSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "messages" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	provName, key, ok := decodeID(id)
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	for _, p := range providers {
		if p.Name() == provName {
			msgs, err := p.Messages(key)
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				writeJSON(w, map[string]any{"items": []Message{}})
				return
			}
			writeJSON(w, map[string]any{"items": msgs})
			return
		}
	}
	http.NotFound(w, r)
}

// ---------- helpers ----------

func randToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func logMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
