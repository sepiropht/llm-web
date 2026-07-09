#!/usr/bin/env bash
set -euo pipefail
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PORT="${PORT:-18800}"
HOST="${HOST:-0.0.0.0}"
command -v go >/dev/null || { echo "❌ Go requis (>= 1.22)"; exit 1; }
echo "🔨 Build…"; (cd "$REPO_DIR" && go build -o llmweb .)
mkdir -p ~/.llm-web ~/.config/systemd/user
[ -f ~/.llm-web/token ] || head -c 18 /dev/urandom | base64 | tr -d '/+=' > ~/.llm-web/token
printf 'LLMWEB_TOKEN=%s\n' "$(cat ~/.llm-web/token)" > ~/.llm-web/env
cat > ~/.config/systemd/user/llm-web.service <<UNIT
[Unit]
Description=LLM Web — toutes tes sessions, tous tes LLM
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$REPO_DIR
EnvironmentFile=-%h/.llm-web/env
EnvironmentFile=-%h/.llm-web/keys.env
Environment=PATH=%h/.local/bin:%h/.kimi-code/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=$REPO_DIR/llmweb -port $PORT -host $HOST -no-auth
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable --now llm-web
loginctl enable-linger "$USER" 2>/dev/null || echo "⚠️  linger non activé (le service s'arrêtera à la déconnexion)"
sleep 2
systemctl --user is-active --quiet llm-web \
  && echo "✅ LLM Web actif sur le port $PORT" \
  || { echo "❌ échec :"; journalctl --user -u llm-web -n 20 --no-pager; exit 1; }
