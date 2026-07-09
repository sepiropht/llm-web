# LLM Web

A single web app that gathers **all your sessions from every LLM CLI installed** on a
machine into one Kimi / ChatGPT-style chat interface. Built for mobile.

A universal equivalent of `kimi web`, but multi-provider.

## Why do you need this?

I mostly run coding agents (Claude Code, Kimi, Grok, …) on a **VPS**. The problem is
getting back to them from a **phone**: `ssh + claude` in a mobile terminal is painful —
tiny text, no scrollback, fiddly keyboard, and every agent stores its history in its own
place so you can never find that conversation from last week.

LLM Web fixes that. Point it at your VPS, open the URL on your phone, and you get a clean,
touch-friendly interface over **all** your agents: browse every past session, resume the
one you need, or start a new chat with any installed model — no SSH, no terminal gymnastics.

## What it does

- **Aggregates the sessions** of every detected LLM CLI:
  - **Claude Code** — `~/.claude/projects/*/*.jsonl`
  - **Kimi Code** — `~/.kimi-code/sessions/wd_*/ses_*/`
  - **Grok (xAI CLI)** — `~/.grok/sessions/<cwd>/<uuid>/`
  - **Codex** — `~/.codex/sessions/`
  - **Gemini** — `~/.gemini/tmp/*/logs.json`
- **Live chat** with any installed LLM (new chat or resume a session):
  - Claude, Kimi, Grok, Codex (CLI, with streaming + resume `--resume` / `-S` / `-r`)
  - Qwen (local llama.cpp model)
  - Mistral, and Grok falls back to the xAI API if the CLI is absent (key required)
- ChatGPT-style UI: sessions grouped by date, search, per-LLM filters, markdown rendering,
  collapsible reasoning and tool-call blocks, light/dark theme, **mobile responsive**,
  auto-refresh.

## Run

```bash
~/llm-web.sh
```

The script builds if needed, generates a persistent token (stable URL), and exposes the
server on all interfaces (mobile access over a VPN like netbird). It prints the URL.

### Install as a service (recommended for a VPS)

```bash
cd ~/code/go/llm-web
./install.sh                    # safe defaults: token required, agents ask before acting
NO_AUTH=1 BYPASS=1 ./install.sh # personal box behind a VPN: no token, agents auto-run
```

Installs a systemd user service (survives reboot via linger). Options are remembered in
`~/.llm-web/env`; edit that file and `systemctl --user restart llm-web` to change them.

### Manually

```bash
go build -o llmweb .
./llmweb -port 18800                 # localhost only
./llmweb -port 18800 -host 0.0.0.0   # exposed (network / mobile access)
```

Flags: `-port`, `-host` (empty = 127.0.0.1; `0.0.0.0` = all interfaces), `-token`,
`-no-auth`, `-bypass-permissions`.

## Auth

Bearer token. Open the URL with `#token=…` (the frontend stores it, then sends it as
`Authorization: Bearer`). All `/api/*` routes require it; the static UI does not.

With `-no-auth`, clients on a **private/VPN network** (loopback, RFC1918, CGNAT `100.64/10`
used by netbird/tailscale) skip the token; public clients still need it. This is what makes
the phone-over-VPN flow tokenless while staying safe from the open internet.

## Permissions (tool execution)

When you chat with Claude it may want to run tools (Bash, file writes…). Two modes, chosen
in the composer:

- **🔒 Ask** — Claude pauses before each action and the UI shows an *Allow / Deny* popup
  (bidirectional `--input-format stream-json` control protocol). This is the **safe default**.
- **⚡ Auto** — Claude runs without asking (`--permission-mode bypassPermissions`).

Server-side safety rule: **Auto is only available when the server is started with
`-bypass-permissions`**. A clone launched without that flag always forces "Ask", whatever the
client requests — so a public deployment is safe by default. (Kimi already runs tools in `-p`
mode, with no permission channel.)

## API keys (Grok / Mistral)

Put your keys in `~/.llm-web/keys.env`:

```bash
GROK_API_KEY=xai-...
MISTRAL_API_KEY=...
```

They enable "new chat" for those providers.

## API (shaped like kimi web's)

- `GET  /api/v1/providers` — detected LLMs + capabilities
- `GET  /api/v1/sessions?q=&provider=&archived=1&limit=` — aggregated sessions
- `GET  /api/v1/sessions/{id}/messages` — a session's messages
- `POST /api/v1/chat` — streaming chat (SSE) `{provider, message, native_id?, cwd?, mode?}`;
  events: `run`, `token`, `tool`, `ask`, `session`, `error`, `done`
- `POST /api/v1/permission` — answer a tool request `{run_id, request_id, allow}`
- `GET  /api/v1/config`

## Architecture

Single Go binary, `net/http` stdlib, embedded UI (`//go:embed`). One adapter per provider
(`providers.go`) exposing `List()` / `Messages()`; chat streaming lives in `chat.go`.
Adding an LLM = adding an adapter.
