# Roadmap & definition of done

Stages are sequential; do not start Stage 3+ until Stage 1 is confirmed against the real Wispr MCP.

## Stage 1 — Wispr MCP (highest risk)

- [x] `login` command: discovery → DCR → Authorization Code + PKCE → store refresh token.
- [x] MCP Streamable HTTP client: `initialize`, `tools/list`, `tools/call` (`bot tools`, `bot call`).
- [ ] Record real `search_meetings` / `get_meeting` responses as sanitized fixtures in `internal/wispr/testdata/`.
- [ ] Resolve every **VERIFY** item in [wispr-mcp.md](wispr-mcp.md) and update the doc.
- Exit: list of recent meetings printed by CLI, token survives restart and refresh.

## Stage 2 — Telegram

- [x] `sendMessage` (HTML), 429 handling, timeouts.
- [x] `telegram-setup`: capture chat id on `/start` (needs your bot token, see `.env.example`).
- [ ] Test message to the allowed chat only.

## Stage 3 — Meeting processing

- [x] Summary parser: decisions, next steps, assignees, empty-summary handling.
- [x] Formatter with HTML escaping and 4096-char limit.
- [ ] `Extractor` interface (deferred to Stage 4: `formatter.Parse` is the Markdown implementation; add the interface where `meeting` consumes it).

## Stage 4 — Automation

- [x] SQLite store, first-run watermark, dedup.
- [x] Poll loop (2 min), pagination, per-meeting retry, graceful shutdown (`bot run`).

## Stage 5 — Finalization

- [ ] Unit tests (below), `Dockerfile`, `.env.example`, README run instructions.

## Unit tests (mocks for Wispr and Telegram)

1. Task extraction from summary (with/without assignees, localized headings).
2. Telegram message formatting (escaping, empty sections omitted, length limit).
3. Duplicate protection (second tick sends nothing).
4. Empty/missing summary.
5. Telegram send failure (meeting stays unprocessed, loop continues, retried later).
6. Restart recovery (new service instance on same SQLite file does not resend; first run sends nothing).

## MVP done when

- Connects to Wispr MCP and lists finished meetings.
- Detects new meetings, extracts summary and tasks.
- Sends to the personal Telegram chat; no duplicates on re-poll.
- Continues correctly after restart.
- Runs locally and in Docker.

## Out of scope for MVP

LLM processing, group chats, task management commands, web UI, webhooks, other note sources, transcripts in chat.
