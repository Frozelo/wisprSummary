# Telegram integration

Uses the [Telegram Bot API](https://core.telegram.org/bots/api) over plain HTTPS (`net/http`).

## Setup flow

1. Create a bot with @BotFather, put the token in `TELEGRAM_BOT_TOKEN` (env / local `.env`, never committed).
2. Run `bot telegram-setup`, send `/start` to the bot from your personal account. The command long-polls `getUpdates`, prints the chat id (private chats only), and exits.
3. Put it in `TELEGRAM_CHAT_ID`. From then on the daemon sends only to that chat.

## Behavior

- Method: `sendMessage` with `parse_mode=HTML`, `disable_web_page_preview=true`.
- All dynamic text is escaped with `html.EscapeString`; only a fixed set of tags is emitted (`<b>`, `<i>`, `<a>`).
- Limit: 4096 characters per message (after entity parsing). Formatter truncates optional sections first; if still too long, splits into several messages at section boundaries.
- Errors: 429 → wait `parameters.retry_after`; 5xx/network → backoff retry; 400/403 (bad chat, bot blocked) → non-retryable, logged without leaking the token (never log request URLs: the token is in the path).
- The daemon does not read incoming updates, has no commands and no group support.

## Message template

```
<b>Встреча:</b> {title}
<b>Дата:</b> {dd.mm.yyyy}

<b>Краткие итоги:</b>
{summary excerpt}

<b>Принятые решения:</b>
• ...

<b>Задачи:</b>
<b>{assignee}:</b>
• ...

<b>Материалы:</b>
<a href="{share_link}">Заметка в Wispr Flow</a>
```

Sections without content are omitted. Labels are configurable constants (Russian by default, matching the request).
