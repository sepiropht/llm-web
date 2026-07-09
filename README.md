# LLM Web

Une app web unique qui rassemble **toutes tes sessions de tous les LLM installés** sur la machine,
dans une interface de chat à la Kimi / ChatGPT. Pensée pour le mobile.

Équivalent universel de `kimi web`, mais multi-provider.

## Ce que ça fait

- **Agrège les sessions** de tous les CLI LLM détectés :
  - **Claude Code** — `~/.claude/projects/*/*.jsonl`
  - **Kimi Code** — `~/.kimi-code/sessions/wd_*/ses_*/`
  - **Gemini** — `~/.gemini/tmp/*/logs.json`
- **Chat live** avec n'importe quel LLM installé (nouveau chat ou reprise de session) :
  - Claude, Kimi (CLI, avec streaming + reprise `--resume` / `-S`)
  - Qwen (modèle local llama.cpp)
  - Grok (xAI) et Mistral (API — clé requise)
- UI type ChatGPT : sidebar des sessions groupées par date, recherche, filtres par LLM,
  rendu markdown, blocs de raisonnement et appels d'outils repliables, thème clair/sombre,
  **responsive mobile**, rafraîchissement auto.

## Lancer

```bash
~/llm-web.sh
```

Le script compile si besoin, génère un token persistant (URL stable), et expose le serveur
sur toutes les interfaces (accès mobile via netbird). Il affiche l'URL avec le token.

### Manuellement

```bash
cd ~/code/go/llm-web
go build -o llmweb .
./llmweb -port 18800                 # localhost seulement
./llmweb -port 18800 -host 0.0.0.0   # exposé (accès réseau/mobile)
```

Options : `-port`, `-host` (vide = 127.0.0.1 ; `0.0.0.0` = toutes interfaces), `-token`.

## Auth

Token bearer. Ouvre l'URL avec `#token=…` (le front le mémorise puis l'envoie en `Authorization: Bearer`).
Toutes les routes `/api/*` l'exigent ; l'UI statique non.

## Clés API (Grok / Mistral)

Mets tes clés dans `~/.llm-web/keys.env` :

```bash
GROK_API_KEY=xai-...
MISTRAL_API_KEY=...
```

Elles activent le "nouveau chat" pour ces providers.

## API (façonnée comme celle de kimi web)

- `GET  /api/v1/providers` — LLM détectés + capacités
- `GET  /api/v1/sessions?q=&provider=&archived=1&limit=` — sessions agrégées
- `GET  /api/v1/sessions/{id}/messages` — messages d'une session
- `POST /api/v1/chat` — chat en streaming (SSE) `{provider, message, native_id?, cwd?}`
- `GET  /api/v1/config`

## Architecture

Binaire Go unique, `net/http` stdlib, UI embarquée (`//go:embed`). Un adaptateur par provider
(`providers.go`) expose `List()` / `Messages()` ; le streaming de chat est dans `chat.go`.
Ajouter un LLM = ajouter un adaptateur.
