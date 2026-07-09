#!/usr/bin/env bash
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "🔨 Build…"; go build -o llmweb .
echo "🔄 Restart…"; systemctl --user restart llm-web
sleep 1
systemctl --user is-active --quiet llm-web && echo "✅ llm-web actif" \
  || { echo "❌ échec :"; journalctl --user -u llm-web -n 20 --no-pager; exit 1; }
