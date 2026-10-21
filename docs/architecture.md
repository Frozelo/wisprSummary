# Architecture

Single Go binary, one process, SQLite file for state. No queues, no extra services.

```
            every 2 min
 ┌────────┐  search_meetings / get_meeting   ┌──────────────┐
 │ Wispr  │◄─────────────────────────────────│              │
 │  MCP   │       OAuth 2.1 + PKCE           │  meeting     │   sendMessage    ┌──────────┐
 └────────┘                                  │  service     │─────────────────►│ Telegram │
                                             │  (poll loop) │                  │ Bot API  │
 ┌────────┐  processed IDs, tokens           │              │                  └──────────┘
 │ SQLite │◄────────────────────────────────►│              │
 └────────┘                                  └──────┬───────┘
                                                    │ formatter (pure funcs)
```

## Layout

```
cmd/bot/main.go            wiring, config, signal handling (context cancel), subcommands
internal/config/           env parsing + validation
internal/wispr/            OAuth (discovery, DCR, PKCE, refresh) + MCP Streamable HTTP client
internal/telegram/         sendMessage, getUpdates (/start → chat id), rate-limit handling
internal/meeting/          poll loop, dedup, retry; depends on small interfaces
internal/formatter/        summary → tasks/decisions → Telegram HTML (pure, no I/O)
internal/storage/          SQLite: processed meetings, oauth state, kv
```

Subcommands of the one binary: `run` (daemon, default), `login` (one-off Wispr OAuth), `telegram-setup` (print chat id after `/start`).

## Interfaces (defined where consumed, in `meeting`)

```go
type MeetingSource interface {
    ListRecent(ctx, since time.Time, cursor string) (page MeetingPage, err error)
    Get(ctx, id string) (Meeting, error)
}
type Notifier interface { Send(ctx, html string) error }
type Store interface {
    Processed(ctx, id string) (bool, error)
    MarkProcessed(ctx, id string, at time.Time) error
    // first-run watermark
    Watermark(ctx) (time.Time, bool, error)
    SetWatermark(ctx, t time.Time) error
}
```

Future AI processing plugs in behind one more interface in `formatter`/`meeting`:

```go
type Extractor interface { Extract(ctx, Meeting) (Digest, error) }
```

MVP implementation: `MarkdownExtractor` (no LLM). A later `LLMExtractor` swaps in without touching the rest.

## Processing algorithm

1. Tick every `POLL_INTERVAL` (default 2m); skip tick if previous still running.
2. `search_meetings` (newest first) with `since = watermark − lookback`; follow `next_cursor` until `has_more=false`, a page is entirely older than the window, or the 1000-cap.
3. For each own meeting that is *done* (see below) and not in SQLite: `get_meeting` (summary only, no transcript).
4. Extract → format → `Send`.
5. On success `MarkProcessed`. On failure log, leave unprocessed (retry next tick, with capped attempts/backoff per meeting). One meeting's failure never aborts the loop.
6. Advance watermark to the newest fully handled meeting start.

**"Done" definition:** `finalized == true` if the API returns it; otherwise `end` is in the past and a non-empty summary exists. **VERIFY** in Stage 1 (see `wispr-mcp.md`).

**First run:** no watermark → set watermark = now, mark nothing, send nothing (config `BACKFILL_HOURS=0` default; optionally >0 to send recent ones).

**Recovery after downtime:** watermark persists; pagination walks back to the watermark so nothing is skipped; SQLite prevents duplicates. Exactly-once is best effort: a crash between Telegram send and `MarkProcessed` can yield one duplicate (accepted; documented).

**Late edits:** a meeting is sent once only. Later summary modifications are ignored in MVP.

## Watermark (as implemented)

Wispr lists meetings by `modified_at`, newest first, so the service keeps one watermark `W` in SQLite and scans pages down to `W`. After a successful tick `W = max(W, min(tickStart − 10m, oldest pending meeting))`, never older than `tickStart − LOOKBACK`. "Pending" means not finalized, summary not ready, or last send failed. First run sets `W = now − BACKFILL_HOURS`, so history is not replayed. Meetings with a finalized flag but an empty summary wait 10 minutes after they end, then are skipped and marked processed (no empty notifications). Shared (someone else's) meetings are ignored.

## Retries & errors

- HTTP client timeouts (connect/TLS 10s, total 30s per request).
- Retry with exponential backoff + jitter on 5xx/network; honor `Retry-After` on Telegram 429 and Wispr "too many requests" (≥60s).
- 401 from Wispr → refresh token once; persistent failure → log + single Telegram alert, keep polling.
- Telegram message > 4096 chars: formatter truncates sections (never splits mid-tag); fall back to splitting into multiple messages.

## Telegram message formatting

Parse-mode HTML; every dynamic string passes through `html.EscapeString`. Sections, in order, each omitted if empty: title/date, short summary, decisions, tasks grouped by assignee, link. Only the first paragraph(s) of the summary are used (not the whole summary). No transcripts. Tasks are only extracted from the summary's next-steps section; nothing is invented. Heading detection is multilingual-tolerant (`Next steps`, `Следующие шаги`, `Action items`, `Решения`, …) and table-driven so it is easy to extend.

## Storage (SQLite, `modernc.org/sqlite` — pure Go, CGO-free for a tiny Docker image)

```sql
processed_meetings(id TEXT PRIMARY KEY, title TEXT, processed_at TEXT)
kv(key TEXT PRIMARY KEY, value BLOB)   -- watermark, telegram chat id, oauth client/tokens
```

Tokens are in `kv` or a separate `0600` file (decide in Stage 1); never in logs or env dumps.

## Security

- Secrets only via env / mounted data volume; `.env`, `*.db`, `data/` in `.gitignore`.
- No listening HTTP server, except a short-lived loopback listener during `login`.
- Telegram: bot only sends to `TELEGRAM_CHAT_ID`; updates from other chats are ignored (except `/start` replying with that chat's own id in `telegram-setup` mode).
- Logging via `log/slog`; redact tokens; never log meeting content.

## Configuration (env)

| Var | Default | Purpose |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | — | required |
| `TELEGRAM_CHAT_ID` | — | allowed chat (from `telegram-setup`) |
| `DATA_DIR` | `./data` | SQLite + token storage |
| `POLL_INTERVAL` | `2m` | |
| `LOOKBACK` | `48h` | search window safety margin |
| `BACKFILL_HOURS` | `0` | meetings to send on first run |
| `WISPR_MCP_URL` | `https://api.wisprflow.ai/connect/mcp` | |
| `LOG_LEVEL` | `info` | |

## Dependencies

Standard library for HTTP/JSON/OAuth/slog; `modernc.org/sqlite` for storage. MCP client hand-written over `net/http` unless the official Go MCP SDK proves clearly simpler (decide in Stage 1).
