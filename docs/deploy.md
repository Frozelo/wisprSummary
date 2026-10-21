# Deploying on a server (Docker)

The service is a single container with no inbound ports; it only calls Wispr and Telegram. Any small VPS works (256–512 MB RAM). Pick a location from which `api.telegram.org` is reachable — check before paying for a month:

```bash
curl -s -o /dev/null -w "%{http_code}\n" https://api.telegram.org          # 302/404 = reachable
curl -s -o /dev/null -w "%{http_code}\n" https://api.wisprflow.ai/connect/mcp   # 401/405 = reachable
```

## 1. Prepare on your own computer

```bash
cp .env.example .env              # set TELEGRAM_BOT_TOKEN
go run ./cmd/bot login            # Wispr OAuth in the browser -> data/wispr_token.json
go run ./cmd/bot telegram-setup   # send /start to the bot, put TELEGRAM_CHAT_ID into .env
go run ./cmd/bot telegram-test    # optional: delivery check
```

## 2. Copy to the server

Copy `docker-compose.yml`, `.env` and `data/wispr_token.json` to the server (for example with `scp`). Keep `.env` private (`chmod 600 .env`).

Image: `ghcr.io/frozelo/wisprsummary:latest` is published by the `docker` workflow. If the package is private, either make it public in the repository's package settings or run `docker login ghcr.io` on the server with a token that has `read:packages`. To build on the server instead: `docker compose up -d --build` from a clone of the repository.

## 3. Seed the Wispr session into the volume and start

The container runs as a non-root user (uid 65532), so the file must be owned by it:

```bash
docker volume create wispr-bot-data
docker run --rm -v wispr-bot-data:/data -v "$PWD:/src:ro" busybox sh -c \
  'cp /src/wispr_token.json /data/ && chown 65532:65532 /data/wispr_token.json && chmod 600 /data/wispr_token.json'
docker compose up -d
docker compose logs -f
```

The first start logs `first run: only meetings modified after now are sent`; nothing from your history is sent. Set `BACKFILL_HOURS` in `.env` before the first start to also send recent meetings.

## 4. Operate

| Task | Command |
|---|---|
| Logs | `docker compose logs -f` |
| Update | `docker compose pull && docker compose up -d` |
| Stop | `docker compose down` (state stays in the `wispr-bot-data` volume) |
| Back up state | `docker run --rm -v wispr-bot-data:/data -v "$PWD:/b" busybox tar -C /data -czf /b/state-backup.tgz .` |
| Re-login | run `bot login` on your computer, seed the new `wispr_token.json` as in step 3, `docker compose restart` |

If the Wispr session expires the service sends a single Telegram message asking for `bot login`.

## Only one runner at a time

Wispr may rotate the refresh token on every refresh, so two places using copies of the same session can invalidate each other, and both would also send every meeting. When you move to a server, **disable the GitHub Actions poller** and stop any local `bot run`:

```bash
gh workflow disable poll.yml
```
