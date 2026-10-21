// Package telegram is a minimal Telegram Bot API client (sendMessage, getUpdates).
//
// The bot token is part of every request URL, so URLs must never reach logs or
// error messages; transport errors are unwrapped to drop them.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	apiBase     = "https://api.telegram.org"
	MaxTextLen  = 4096
	maxAttempts = 3
	maxRetryCap = 60 * time.Second
)

// ErrTooLong is returned when the text exceeds Telegram's message limit.
var ErrTooLong = errors.New("telegram: message exceeds 4096 characters")

// APIError is a non-retryable error reported by the Bot API (e.g. bad chat, bot blocked).
type APIError struct {
	Code        int
	Description string
}

func (e *APIError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Description) }

type Client struct {
	token   string
	baseURL string
	hc      *http.Client
	sleep   func(context.Context, time.Duration) error
}

func New(token string, hc *http.Client) *Client {
	return &Client{token: token, baseURL: apiBase, hc: hc, sleep: sleepCtx}
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Button is an inline keyboard button; Data is returned in a callback query.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// Keyboard is an inline keyboard: rows of buttons. Nil means no keyboard.
type Keyboard [][]Button

// CallbackQuery is a button press.
type CallbackQuery struct {
	ID   string `json:"id"`
	Data string `json:"data"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message *struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

// Update is the part of a Bot API update we use.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
	Message       *struct {
		Text string `json:"text"`
		From struct {
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
}

// SendMessage sends HTML text to chatID, retrying on 429 (honoring retry_after),
// 5xx and dial errors.
func (c *Client) SendMessage(ctx context.Context, chatID int64, html string) error {
	_, err := c.SendMessageKB(ctx, chatID, html, nil)
	return err
}

// SendMessageKB is SendMessage with an optional inline keyboard; it returns the
// id of the sent message so it can be edited later.
func (c *Client) SendMessageKB(ctx context.Context, chatID int64, html string, kb Keyboard) (int64, error) {
	if utf8.RuneCountInString(html) > MaxTextLen {
		return 0, ErrTooLong
	}
	body := map[string]any{
		"chat_id":              chatID,
		"text":                 html,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if len(kb) > 0 {
		body["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	var raw json.RawMessage
	// sendMessage is not idempotent: only retry when the request provably never reached Telegram.
	err := c.withRetry(ctx, func() error {
		r, err := c.call(ctx, "sendMessage", body, false)
		raw = r
		return err
	})
	if err != nil {
		return 0, err
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &sent)
	return sent.MessageID, nil
}

// EditMessage replaces the text and keyboard of a sent message. Editing to
// identical content is not an error.
func (c *Client) EditMessage(ctx context.Context, chatID, messageID int64, html string, kb Keyboard) error {
	if utf8.RuneCountInString(html) > MaxTextLen {
		return ErrTooLong
	}
	body := map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"text":                 html,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
		"reply_markup":         map[string]any{"inline_keyboard": kb}, // empty keyboard removes it
	}
	if kb == nil {
		body["reply_markup"] = map[string]any{"inline_keyboard": [][]Button{}}
	}
	err := c.withRetry(ctx, func() error {
		_, err := c.call(ctx, "editMessageText", body, true)
		return err
	})
	if ae, ok := errors.AsType[*APIError](err); ok && strings.Contains(ae.Description, "message is not modified") {
		return nil
	}
	return err
}

// AnswerCallback acknowledges a button press (stops the client's spinner) and
// optionally shows a short notice. A stale query is not worth failing over.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	_, err := c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, true)
	return err
}

// GetUpdates long-polls for new messages. timeout is in seconds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	body := map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}
	var raw json.RawMessage
	err := c.withRetry(ctx, func() error {
		r, err := c.call(ctx, "getUpdates", body, true)
		raw = r
		return err
	})
	if err != nil {
		return nil, err
	}
	var ups []Update
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, fmt.Errorf("telegram: decode updates: %w", err)
	}
	return ups, nil
}

// retryable marks errors worth another attempt. after is a server-requested
// wait (429); zero means use the default backoff.
type retryable struct {
	err   error
	after time.Duration
}

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

func (c *Client) withRetry(ctx context.Context, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		r, ok := errors.AsType[*retryable](err)
		if !ok {
			return err
		}
		// Retrying before Telegram's retry_after only extends the flood ban:
		// give up and let the caller back off instead.
		if attempt == maxAttempts || r.after > maxRetryCap {
			return r.err
		}
		wait := r.after
		if wait == 0 {
			wait = time.Duration(attempt) * 2 * time.Second
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// call performs one API request. idempotent says whether repeating the request
// after an ambiguous transport failure (timeout, reset) is harmless.
func (c *Client) call(ctx context.Context, method string, body any, idempotent bool) (json.RawMessage, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("telegram: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		neverSent := isDialError(err)
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // drop the URL: it contains the token
		}
		err = fmt.Errorf("telegram: %s: %w", method, err)
		if idempotent || neverSent {
			return nil, &retryable{err: err}
		}
		return nil, fmt.Errorf("%w (delivery state unknown, not retried)", err)
	}
	defer resp.Body.Close()

	var r apiResponse
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err := json.Unmarshal(data, &r); err != nil {
		herr := fmt.Errorf("telegram: %s: http %d, bad response", method, resp.StatusCode)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			return nil, &retryable{err: herr, after: time.Duration(secs) * time.Second}
		case resp.StatusCode >= 500:
			return nil, &retryable{err: herr}
		}
		return nil, herr
	}
	if r.OK {
		return r.Result, nil
	}
	apiErr := &APIError{Code: r.ErrorCode, Description: r.Description}
	switch {
	case r.ErrorCode == http.StatusTooManyRequests:
		return nil, &retryable{err: apiErr, after: time.Duration(r.Parameters.RetryAfter) * time.Second}
	case r.ErrorCode >= 500:
		return nil, &retryable{err: apiErr}
	}
	return nil, apiErr
}

// isDialError reports a failure before any request bytes could reach the server.
func isDialError(err error) bool {
	oe, ok := errors.AsType[*net.OpError](err)
	return ok && oe.Op == "dial"
}

// Sender identifies who sent a /start message.
type Sender struct {
	ChatID int64
	Name   string
}

// WaitForStart waits for a /start sent to the bot in a private chat after this
// call began (older pending updates are discarded) and returns that chat id.
// confirm must approve each sender; the bot is public, so a stranger could
// otherwise get registered. Only confirmed chats receive a reply.
func (c *Client) WaitForStart(ctx context.Context, confirm func(Sender) bool) (int64, error) {
	var offset int64
	old, err := c.GetUpdates(ctx, -1, 0) // -1 = only the most recent pending update
	if err != nil {
		return 0, err
	}
	for _, u := range old {
		offset = max(offset, u.UpdateID+1)
	}
	for {
		ups, err := c.GetUpdates(ctx, offset, 30)
		if err != nil {
			return 0, err
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.Chat.Type != "private" || !isStart(m.Text) {
				continue
			}
			s := Sender{ChatID: m.Chat.ID, Name: displayName(m.From.FirstName, m.From.LastName, m.From.Username)}
			if !confirm(s) {
				continue
			}
			_ = c.SendMessage(ctx, s.ChatID, "Chat registered. Set TELEGRAM_CHAT_ID in the service config to receive meeting summaries here.")
			return s.ChatID, nil
		}
	}
}

func isStart(text string) bool {
	f := strings.Fields(text)
	if len(f) == 0 {
		return false
	}
	cmd, _, _ := strings.Cut(f[0], "@") // /start@BotName
	return cmd == "/start"
}

func displayName(first, last, username string) string {
	name := strings.TrimSpace(first + " " + last)
	if username != "" {
		name += " (@" + username + ")"
	}
	if name == "" {
		return "unknown"
	}
	return strings.TrimSpace(name)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Chat talks to one fixed chat; it is the only chat the service uses.
type Chat struct {
	C  *Client
	ID int64
}

func (c Chat) Send(ctx context.Context, html string, kb Keyboard) (int64, error) {
	return c.C.SendMessageKB(ctx, c.ID, html, kb)
}

func (c Chat) Edit(ctx context.Context, messageID int64, html string, kb Keyboard) error {
	return c.C.EditMessage(ctx, c.ID, messageID, html, kb)
}

func (c Chat) Updates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	return c.C.GetUpdates(ctx, offset, timeoutSec)
}

func (c Chat) Answer(ctx context.Context, callbackID, text string) error {
	return c.C.AnswerCallback(ctx, callbackID, text)
}
