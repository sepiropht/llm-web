#!/usr/bin/env bash
# Installe LLM Web comme service utilisateur systemd.
#
# Défauts SÛRS : token requis, et les agents demandent avant d'exécuter un outil.
# Pour une machine perso derrière un VPN, tu peux assouplir :
#
#   NO_AUTH=1 BYPASS=1 ./install.sh
#
#   NO_AUTH=1  → pas de token pour les clients privés/VPN (loopback, RFC1918, CGNAT)
#   BYPASS=1   → autorise le mode « Auto » (les agents exécutent sans demander)
#   PORT=…     → port d'écoute (défaut 18800)
#   HOST=…     → interface (défaut 0.0.0.0 ; mets 127.0.0.1 pour rester local)
#
# Ces choix sont mémorisés dans ~/.llm-web/env : pour les changer ensuite,
# édite ce fichier puis `systemctl --user restart llm-web`.
set -euo pipefail
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PORT="${PORT:-18800}"
HOST="${HOST:-0.0.0.0}"

command -v go >/dev/null || { echo "❌ Go requis (>= 1.22)"; exit 1; }

# ---------- détection des agents ----------
# Un agent est utilisable s'il a son binaire (chat) et/ou un dossier de sessions
# (historique). Les deux cas sont valables : un agent fraîchement installé n'a
# pas encore de sessions, un agent désinstallé laisse son historique.
echo "🔎 Agents détectés :"
found=0
declare -A SESS_DIR=(
  [claude]="$HOME/.claude/projects"
  [kimi]="$HOME/.kimi-code/sessions"
  [grok]="$HOME/.grok/sessions"
  [codex]="$HOME/.codex/sessions"
  [gemini]="$HOME/.gemini/tmp"
)
for a in claude kimi grok codex gemini qwen; do
  bin=no; sess=no
  command -v "$a" >/dev/null 2>&1 && bin=yes
  [ -n "${SESS_DIR[$a]:-}" ] && [ -d "${SESS_DIR[$a]:-/nonexistent}" ] && sess=yes
  if [ "$bin" = yes ] && [ "$sess" = yes ]; then echo "   ✓ $a — chat + historique"; found=$((found+1))
  elif [ "$bin" = yes ];               then echo "   ✓ $a — chat (pas encore d'historique)"; found=$((found+1))
  elif [ "$sess" = yes ];              then echo "   ○ $a — historique seul (binaire absent)"; found=$((found+1))
  fi
done
[ -n "${GROK_API_KEY:-}${XAI_API_KEY:-}" ] && { echo "   ✓ grok — via API xAI"; found=$((found+1)); }
[ -n "${MISTRAL_API_KEY:-}" ]              && { echo "   ✓ mistral — via API"; found=$((found+1)); }
if [ "$found" -eq 0 ]; then
  echo "   ⚠️  aucun agent trouvé — l'app démarrera mais sera vide."
  echo "      Installe claude / kimi / grok / codex / gemini, puis relance ce script."
fi

echo "🔨 Build…"; (cd "$REPO_DIR" && go build -o llmweb .)

mkdir -p ~/.llm-web ~/.config/systemd/user
[ -f ~/.llm-web/token ] || head -c 18 /dev/urandom | base64 | tr -d '/+=' > ~/.llm-web/token

# ---------- options, sûres par défaut ----------
ARGS=""
[ "${NO_AUTH:-0}" = "1" ] && ARGS="$ARGS -no-auth"
[ "${BYPASS:-0}"  = "1" ] && ARGS="$ARGS -bypass-permissions"
ARGS="${ARGS# }"

{
  printf 'LLMWEB_TOKEN=%s\n' "$(cat ~/.llm-web/token)"
  printf 'LLMWEB_ARGS=%s\n'  "$ARGS"
} > ~/.llm-web/env

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
Environment=PATH=%h/.local/bin:%h/.kimi-code/bin:%h/.grok/bin:%h/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=$REPO_DIR/llmweb -port $PORT -host $HOST \$LLMWEB_ARGS
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
UNIT

systemctl --user daemon-reload
systemctl --user enable --now llm-web
systemctl --user restart llm-web   # `enable --now` ne redémarre pas un service déjà actif
loginctl enable-linger "$USER" 2>/dev/null || echo "⚠️  linger non activé (le service s'arrêtera à la déconnexion)"
sleep 2

if systemctl --user is-active --quiet llm-web; then
  echo ""
  echo "✅ LLM Web actif sur le port $PORT"
  echo "   Sécurité : $([ "${NO_AUTH:-0}" = 1 ] && echo 'token non requis sur réseau privé/VPN' || echo 'token requis')"\
       "· $([ "${BYPASS:-0}" = 1 ] && echo 'mode Auto autorisé' || echo 'les agents demandent avant d'\''agir')"
  echo "   URL : http://localhost:$PORT/#token=$(cat ~/.llm-web/token)"
else
  echo "❌ échec :"; journalctl --user -u llm-web -n 20 --no-pager; exit 1
fi
