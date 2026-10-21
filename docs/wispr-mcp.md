# Wispr Flow MCP integration

Findings from official docs and live probing of the endpoints (2026-10-21). Items marked **VERIFY** must be confirmed in Stage 1 against a real authorized session; nothing here should be treated as a guaranteed contract.

## Sources

- Official guide: <https://docs.wisprflow.ai/articles/9551372685-connect-an-mcp-client-to-wispr-flow-remote-mcp-server>
- Third-party overview (vendor view, not a spec): <https://www.scalekit.com/blog/wispr-flow-mcp-vs-api>
- MCP authorization spec (OAuth 2.1, PKCE, protected resource metadata, DCR)

## Server facts

| Item | Value |
|---|---|
| MCP URL | `https://api.wisprflow.ai/connect/mcp` (no trailing slash) |
| Access | Read-only. Cannot create/edit/share/delete notes or change sharing. |
| Data exposed | Meeting summaries, notes, transcripts, attendees, tasks, scratchpad, calendar events |
| Not exposed | Dictations, Notetaker chat history |
| Platforms | Mac and Windows, Notetaker + MCP enabled on the account |
| Sign-in | Google / Apple / Microsoft / SSO. Email+password only works if login returns to the authorize step. Finish browser sign-in within 5 minutes. |
| Auth scope | Account-wide, no per-meeting consent |
| Rate limits | Thresholds undocumented. On "Too many requests": wait ~1 min, retry. |
| Disconnect | Only by removing the connection in the client; Wispr has no disconnect UI. |

Wispr docs say "no API key or token" — auth is OAuth only. There is no public REST API documented.

## OAuth discovery (observed)

Unauthenticated `POST /connect/mcp` returns:

```
HTTP 401
www-authenticate: Bearer resource_metadata="https://api.wisprflow.ai/.well-known/oauth-protected-resource", scope="openid offline_access"
```

Protected resource metadata (`/.well-known/oauth-protected-resource`):

```json
{"resource":"https://api.wisprflow.ai/connect/mcp",
 "authorization_servers":["https://mcp-auth.wisprflow.com"],
 "scopes_supported":["openid","offline_access"],
 "bearer_methods_supported":["header"]}
```

Authorization server metadata (`https://mcp-auth.wisprflow.com/.well-known/oauth-authorization-server`):

| Field | Value |
|---|---|
| authorization_endpoint | `https://mcp-auth.wisprflow.com/oauth2/authorize` |
| token_endpoint | `https://mcp-auth.wisprflow.com/oauth2/token` |
| registration_endpoint (DCR) | `https://mcp-auth.wisprflow.com/oauth2/register` |
| device_authorization_endpoint | `https://mcp-auth.wisprflow.com/oauth2/device_authorization` |
| grant types | `authorization_code`, `refresh_token`, `urn:ietf:params:oauth:grant-type:device_code` |
| PKCE | `S256` only |
| token auth methods | `none`, `client_secret_post`, `client_secret_basic` |
| client_id_metadata_document_supported | `true` (CIMD) |
| scopes | `openid`, `offline_access`, `email`, `profile` |

### Conclusion: direct Go integration is feasible

The server follows the standard MCP authorization flow, so no undocumented API or workaround is needed:

1. Discover metadata from the 401 challenge (RFC 9728 → RFC 8414).
2. Register a public client via DCR (`token_endpoint_auth_method: none`), once; persist `client_id`.
3. Authorization Code + PKCE (S256), scopes `openid offline_access`, with a loopback redirect URI (`http://127.0.0.1:<port>/callback`) during a one-off `login` command.
4. Persist the refresh token; refresh access tokens automatically in the daemon. `offline_access` is what makes this possible.
5. Call MCP over Streamable HTTP with `Authorization: Bearer <access_token>`.

Alternative if loopback redirect is rejected: device authorization grant (`device_code`) is advertised — useful for Docker/headless login. **VERIFY** which redirect URIs DCR accepts.

### Risks to verify in Stage 1

- Does DCR accept `http://127.0.0.1` redirect URIs? Does the device flow work for a DCR client?
- Refresh token lifetime and rotation (store the new refresh token on every refresh, atomically).
- Access token lifetime.
- Whether the account's login method can complete authorization (see sign-in note above).
- Streamable HTTP details: `initialize` → `Mcp-Session-Id` header handling, `notifications/initialized`, JSON vs SSE responses. Implement with `net/http`; consider the official Go MCP SDK only if hand-rolling proves brittle.
- Exact tool names (Scalekit mentions a `wisprflowmcp_` prefix in its connector; use `tools/list` to confirm).

Fallback if direct OAuth fails: a user-authorized one-time login done in a browser by the user, with the refresh token passed into the service via local config. This is still the same documented OAuth flow, not a bypass. Do not extract tokens from other MCP clients' storage.

## Tools

Observed in a connected MCP client; names and fields must be re-checked via `tools/list` (**VERIFY**).

### `search_meetings`

Input: `query`, `field` (`title|content|both`), `since`, `until` (ISO datetime, half-open), `attendee_emails`, `cursor`, `limit` (default 25, max 200).

Output per meeting: id, title, start/end (UTC), `has_transcript`, `share_link`, up to 5 attendees, excerpt, `folders`, `shared`, `owner`. Newest first. Pagination: `has_more` + `next_cursor` (pass verbatim). Cap 1000 results per query (`truncated=true`).

Notes vs. the original brief:

- The original brief lists `finalized` and `modified_at` in search results; the tool description we saw does not mention them. **VERIFY** the real response. If absent, "meeting is done" must be derived another way (e.g. `end` in the past + summary present, plus a settle delay).
- `since`/`until` filter on meeting start time, not modification time — a meeting finalized later can be missed by a narrow window. Use a generous lookback (e.g. 48h) and rely on SQLite dedup.

### `get_meeting`

Input: `meeting_id`, optional `view_content{start_char,char_limit}`, optional `view_transcript{...}`.

Output: title, start/end, attendees, markdown `content` (bounded, paginated), `summary` (always complete; its next-steps section holds the action items), `share_link`. Transcript only when requested — **the service must never request it** (MVP does not publish transcripts).

Notes:

- No separate `todos` field was seen in the tool description; per the docs, tasks live in the summary's "Next steps" section (localized heading, e.g. «Следующие шаги»). **VERIFY**.
- Shared meetings (`shared=true`) belong to someone else; the MVP should skip them (own meetings only).

## Verified live (2026-10-21)

Confirmed with the Go client against the real server (`bot login`, `bot tools`, `bot call`):

- Direct OAuth works: discovery, DCR, PKCE with loopback redirect, refresh token issued. Access token lifetime is about 7 days.
- Streamable HTTP works with `Authorization: Bearer`; 14 read-only tools are exposed with **unprefixed** names (`search_meetings`, `get_meeting`, ...).
- Tool results carry a parsed `structuredContent` object (preferred over parsing the text block).
- `search_meetings` items: `id, title, content_excerpt, finalized, has_transcript, attendees, start, end, modified_at, share_link` (+ `shared`, `owner`, `folders` for others' notes). **`finalized` and `modified_at` exist.**
- Without `query`, results are ordered by **`modified_at` newest first** (cursor encodes `modified_at|id`), despite `since/until` filtering by meeting start. So a `modified_at` watermark is the right incremental key.
- Page also returns `count`, `has_more`, `next_cursor`, `more`.
- `get_meeting` keys: `id, title, content, summary, finalized, has_transcript, attendees, todos, start, end, modified_at, share_link`. `todos` was an empty list for the meetings tried, `content` (user notes) was empty, and `summary` is markdown using `###` headings. `summary` may be absent/empty for very short recordings.

Summary conventions seen across 15 real meetings (Russian locale): `### Принятые решения` (plain bullets), `### Следующие шаги` / `### Дальнейшие шаги` (bullets as `- (Исполнитель) задача`, sometimes without an assignee), other `###` headings are topics. Long structured notes may use numbered `##` headings. `todos` was empty in all 15. The parser (`internal/formatter/parse.go`) is keyed on these headings and is table-driven.

Still open: `todos` shape when non-empty, which heading names Wispr uses for next steps across languages, refresh-token rotation behaviour (the client already persists rotated tokens).

## Implementation notes

- Never log tokens or request `Authorization` headers.
- Store tokens in a file with `0600` permissions inside a mounted data volume (or encrypted with a key from env). Write atomically (temp file + rename).
- Treat 401 → refresh once → retry; if refresh fails, log `re-login required` and keep running (alert via Telegram once).
- Treat "Too many requests" as backoff (≥ 60 s).
- Read-only usage only. The service must not call anything that could modify notes or sharing (none are exposed anyway).
