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
	"net/http"
	"net/url"
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

// Update is the part of a Bot API update we use.
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
}

// SendMessage sends HTML text to chatID, retrying on 429 (honoring retry_after),
// 5xx and network errors.
func (c *Client) SendMessage(ctx context.Context, chatID int64, html string) error {
	if utf8.RuneCountInString(html) > MaxTextLen {
		return ErrTooLong
	}
	body := map[string]any{
		"chat_id":              chatID,
		"text":                 html,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	return c.withRetry(ctx, func() (time.Duration, error) {
		_, wait, err := c.call(ctx, "sendMessage", body)
		return wait, err
	})
}

// GetUpdates long-polls for new messages. timeout is in seconds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	body := map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message"},
	}
	var raw json.RawMessage
	err := c.withRetry(ctx, func() (time.Duration, error) {
		r, wait, err := c.call(ctx, "getUpdates", body)
		raw = r
		return wait, err
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

// retryable marks errors worth another attempt.
type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

func (c *Client) withRetry(ctx context.Context, fn func() (time.Duration, error)) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var wait time.Duration
		wait, err = fn()
		if err == nil {
			return nil
		}
		if _, ok := errors.AsType[*retryable](err); !ok || attempt == maxAttempts {
			break
		}
		if wait == 0 {
			wait = time.Duration(attempt) * 2 * time.Second
		}
		if err := c.sleep(ctx, min(wait, maxRetryCap)); err != nil {
			return err
		}
	}
	if r, ok := errors.AsType[*retryable](err); ok {
		return r.err
	}
	return err
}

// call performs one API request. The returned duration is a server-requested wait.
func (c *Client) call(ctx context.Context, method string, body any) (json.RawMessage, time.Duration, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, 0, errors.New("telegram: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // drop the URL: it contains the token
		}
		return nil, 0, &retryable{fmt.Errorf("telegram: %s: %w", method, err)}
	}
	defer resp.Body.Close()

	var r apiResponse
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err := json.Unmarshal(data, &r); err != nil {
		if resp.StatusCode >= 500 {
			return nil, 0, &retryable{fmt.Errorf("telegram: %s: http %d", method, resp.StatusCode)}
		}
		return nil, 0, fmt.Errorf("telegram: %s: http %d, bad response", method, resp.StatusCode)
	}
	if r.OK {
		return r.Result, 0, nil
	}
	apiErr := &APIError{Code: r.ErrorCode, Description: r.Description}
	switch {
	case r.ErrorCode == http.StatusTooManyRequests:
		return nil, time.Duration(r.Parameters.RetryAfter) * time.Second, &retryable{apiErr}
	case r.ErrorCode >= 500:
		return nil, 0, &retryable{apiErr}
	}
	return nil, 0, apiErr
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

// Chat sends messages to one fixed chat; it is the only sink the service uses.
type Chat struct {
	C  *Client
	ID int64
}

func (c Chat) Send(ctx context.Context, html string) error { return c.C.SendMessage(ctx, c.ID, html) }
