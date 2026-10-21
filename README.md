# Wispr Flow → Telegram Meeting Assistant

A small Go service that watches your [Wispr Flow](https://wisprflow.ai) Notetaker for newly finished meetings and sends a compact summary (key points, decisions, action items, link) to your personal Telegram chat. No manual steps after a call ends.

> **Status:** MVP complete — `bot run` polls Wispr and delivers summaries to Telegram; runs locally, in Docker, or (for a free trial) on GitHub Actions. See [docs/roadmap.md](docs/roadmap.md).

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

## Quick start (local)

```bash
cp .env.example .env            # set TELEGRAM_BOT_TOKEN (from @BotFather)
go run ./cmd/bot login          # one-off Wispr OAuth in the browser
go run ./cmd/bot telegram-setup # send /start to your bot, confirm, copy the chat id into .env
go run ./cmd/bot telegram-test  # optional: check delivery
go run ./cmd/bot run            # start polling (Ctrl+C to stop)
```

Other commands: `status`, `tools`, `call <tool> [json]`, `preview <meeting-id> [--send]`, `run --once`.

## Docker

```bash
docker compose up -d --build    # builds the image and starts the service
```

The image is a static binary on distroless (non-root); state lives in the `wispr-bot-data` volume. Step-by-step server setup, updates and backups: [docs/deploy.md](docs/deploy.md). CI (`gofmt`, `vet`, `test -race`) runs on every pull request, and the `docker` workflow publishes `ghcr.io/frozelo/wisprsummary` from `main`.

## Try it for free on GitHub Actions

No server needed for a trial: `.github/workflows/poll.yml` runs `bot run --once` every ~5 minutes (GitHub may start scheduled runs 5–15 minutes late) and keeps the state between runs in the Actions cache, **encrypted** with the `STATE_KEY` secret because the repository is public.

Repository secrets: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`, `STATE_KEY` (any random string) and `WISPR_TOKEN_B64` (`base64 < data/wispr_token.json`, used only when there is no saved state). Optional repository variable: `TIMEZONE`.

```bash
gh workflow run poll.yml -f backfill_hours=24   # first run only: also send the last 24h
gh run list --workflow poll.yml                  # check the runs
```

Notes: do not run the bot locally with the same Wispr session at the same time (a refreshed token may invalidate the other copy); if the cache is evicted (7 days unused) the session is re-seeded from `WISPR_TOKEN_B64`, which may require `bot login` again; GitHub disables schedules after 60 days without repository activity. For permanent use run the service on a VPS instead ([docs/deploy.md](docs/deploy.md)) and disable this workflow (`gh workflow disable poll.yml`).

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
- [Deploying on a server](docs/deploy.md)
- [Telegram integration](docs/telegram.md)
- [Roadmap, tests, definition of done](docs/roadmap.md)

## Known limitations

- Task extraction relies on Wispr's own summary ("Next steps" section); there is no LLM in the MVP (an `Extractor` interface is reserved for it).
- A crash between sending and recording a meeting may cause one duplicate.
- Later edits to an already-sent meeting are not re-sent.
