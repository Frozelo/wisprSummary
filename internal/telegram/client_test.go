package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("SECRET-TOKEN", srv.Client())
	c.baseURL = srv.URL
	var sleeps []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	return c, &sleeps
}

func TestSendMessageBody(t *testing.T) {
	var got map[string]any
	var path string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"ok":true,"result":{}}`)
	})
	if err := c.SendMessage(context.Background(), 42, "<b>hi</b>"); err != nil {
		t.Fatal(err)
	}
	if path != "/botSECRET-TOKEN/sendMessage" {
		t.Fatalf("path = %s", path)
	}
	if got["chat_id"] != float64(42) || got["parse_mode"] != "HTML" || got["text"] != "<b>hi</b>" {
		t.Fatalf("body = %v", got)
	}
}

func TestSendMessageRateLimitRetriesAfterDelay(t *testing.T) {
	var n atomic.Int32
	c, sleeps := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`)
			return
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	})
	if err := c.SendMessage(context.Background(), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 || len(*sleeps) != 1 || (*sleeps)[0] != 7*time.Second {
		t.Fatalf("calls=%d sleeps=%v", n.Load(), *sleeps)
	}
}

func TestSendMessageFailureIsNotRetriedOnClientError(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`)
	})
	err := c.SendMessage(context.Background(), 1, "x")
	if ae, ok := errors.AsType[*APIError](err); !ok || ae.Code != 403 {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("calls = %d, want 1", n.Load())
	}
}

func TestSendMessageGivesUpAfterServerErrors(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	if err := c.SendMessage(context.Background(), 1, "x"); err == nil {
		t.Fatal("expected error")
	}
	if n.Load() != maxAttempts {
		t.Fatalf("calls = %d, want %d", n.Load(), maxAttempts)
	}
}

func TestTransportErrorDoesNotLeakToken(t *testing.T) {
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	c.baseURL = "http://127.0.0.1:1" // connection refused
	err := c.SendMessage(context.Background(), 1, "x")
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestMessageTooLong(t *testing.T) {
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("must not call API") })
	err := c.SendMessage(context.Background(), 1, strings.Repeat("я", MaxTextLen+1))
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetUpdates(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":[{"update_id":5,"message":{"text":"/start","chat":{"id":99,"type":"private"}}}]}`)
	})
	ups, err := c.GetUpdates(context.Background(), 0, 0)
	if err != nil || len(ups) != 1 || ups[0].Message.Chat.ID != 99 || ups[0].UpdateID != 5 {
		t.Fatalf("ups=%+v err=%v", ups, err)
	}
}
