# Wispr Flow → Telegram Meeting Assistant

A small Go service that watches your [Wispr Flow](https://wisprflow.ai) Notetaker for newly finished meetings and sends a compact summary (key points, decisions, action items, link) to your personal Telegram chat. No manual steps after a call ends.

> **Status:** Stages 1–4 done — `bot run` polls Wispr and delivers summaries to Telegram. Dockerfile and final polish (Stage 5) are pending; see [docs/roadmap.md](docs/roadmap.md).

## How it works

1. Every 2 minutes the service queries the Wispr Flow remote MCP server (`https://api.wisprflow.ai/connect/mcp`) for recent meetings.
2. For each new, finished meeting it fetches the summary (never the transcript).
3. It extracts decisions and next steps from the summary, formats a Telegram HTML message, and sends it to one allowed chat.
4. The meeting ID is stored in SQLite so nothing is sent twice, even across restarts.

On first start it sends nothing from your history; only meetings finishing afterwards are delivered.

## Stack

Go · Wispr Flow remote MCP (OAuth 2.1 + PKCE, read-only) · Telegram Bot API · SQLite · Docker. No Redis, queues or other services.

## Authentication

Wispr's MCP server uses standard MCP OAuth (discovery, dynamic client registration, authorization code + PKCE, refresh tokens via `offline_access`). There is no API key. You authorize once through a browser with `bot login`; the refresh token is stored in the local data directory (`0600`) and renewed automatically. Access is read-only; the service never modifies notes or sharing settings. Details: [docs/wispr-mcp.md](docs/wispr-mcp.md).

## Planned usage

```bash
cp .env.example .env            # set TELEGRAM_BOT_TOKEN
go run ./cmd/bot login          # one-off Wispr OAuth in the browser
go run ./cmd/bot telegram-setup # send /start to your bot, copy the chat id into .env
go run ./cmd/bot telegram-test  # optional: check delivery
go run ./cmd/bot run            # start polling (Ctrl+C to stop)
```

Docker:

```bash
docker build -t wispr-telegram .
docker run --env-file .env -v "$PWD/data:/data" wispr-telegram
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | – | Bot token from @BotFather |
| `TELEGRAM_CHAT_ID` | – | The only chat messages are sent to |
| `DATA_DIR` | `./data` | SQLite database and OAuth tokens |
| `POLL_INTERVAL` | `2m` | How often to check Wispr |
| `LOOKBACK` | `48h` | Search window safety margin |
| `BACKFILL_HOURS` | `0` | Meetings to send on first run |
| `WISPR_MCP_URL` | `https://api.wisprflow.ai/connect/mcp` | MCP endpoint |
| `TIMEZONE` | system local | IANA zone for the date in messages |
| `LOG_LEVEL` | `info` | |

## Security

- Tokens and `.env` are never committed or logged.
- No public HTTP endpoint (only a short-lived loopback listener during `login`).
- Messages go only to the configured private chat; transcripts are never published.

## Documentation

- [Architecture](docs/architecture.md)
- [Wispr MCP integration & OAuth findings](docs/wispr-mcp.md)
- [Telegram integration](docs/telegram.md)
- [Roadmap, tests, definition of done](docs/roadmap.md)

## Known limitations

- Task extraction relies on Wispr's own summary ("Next steps" section); there is no LLM in the MVP (an `Extractor` interface is reserved for it).
- A crash between sending and recording a meeting may cause one duplicate.
- Later edits to an already-sent meeting are not re-sent.
