package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestRetryAfterAboveCapIsNotRetried(t *testing.T) {
	var n atomic.Int32
	c, sleeps := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":300}}`)
	})
	err := c.SendMessage(context.Background(), 1, "x")
	if ae, ok := errors.AsType[*APIError](err); !ok || ae.Code != 429 {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 1 || len(*sleeps) != 0 {
		t.Fatalf("calls=%d sleeps=%v: must not retry before retry_after", n.Load(), *sleeps)
	}
}

func TestNonJSON429HonorsRetryAfterHeader(t *testing.T) {
	var n atomic.Int32
	c, sleeps := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "<html>slow down</html>")
			return
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	})
	if err := c.SendMessage(context.Background(), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 5*time.Second {
		t.Fatalf("sleeps = %v", *sleeps)
	}
}

func TestSendMessageNotRetriedAfterTimeout(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	})
	c.hc = &http.Client{Timeout: 50 * time.Millisecond}
	err := c.SendMessage(context.Background(), 1, "x")
	if err == nil || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("calls = %d: a possibly delivered message must not be resent", n.Load())
	}
}

func TestSendMessageRetriesDialErrors(t *testing.T) {
	c, sleeps := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	c.baseURL = "http://127.0.0.1:1" // connection refused: the request never left
	if err := c.SendMessage(context.Background(), 1, "x"); err == nil {
		t.Fatal("expected error")
	}
	if len(*sleeps) != maxAttempts-1 {
		t.Fatalf("sleeps = %v, want %d retries", *sleeps, maxAttempts-1)
	}
}

func updatesJSON(updates ...string) string {
	return fmt.Sprintf(`{"ok":true,"result":[%s]}`, strings.Join(updates, ","))
}

func startUpdate(id int, chat int64, typ, text string) string {
	return fmt.Sprintf(`{"update_id":%d,"message":{"text":%q,"from":{"first_name":"Ann","username":"ann"},"chat":{"id":%d,"type":%q}}}`, id, text, chat, typ)
}

func TestWaitForStartIgnoresOldGroupAndRejectedSenders(t *testing.T) {
	var calls atomic.Int32
	var sent []int64
	var offsets []float64
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			sent = append(sent, int64(body["chat_id"].(float64)))
			io.WriteString(w, `{"ok":true,"result":{}}`)
			return
		}
		offsets = append(offsets, body["offset"].(float64))
		switch calls.Add(1) {
		case 1: // pending update from before the command started
			io.WriteString(w, updatesJSON(startUpdate(10, 111, "private", "/start")))
		case 2: // group chat, non-start text, then a stranger, then the owner
			io.WriteString(w, updatesJSON(
				startUpdate(11, -5, "group", "/start"),
				startUpdate(12, 222, "private", "hello"),
				startUpdate(13, 333, "private", "/start"),
				startUpdate(14, 444, "private", "/start@MyBot")))
		}
	})
	var asked []int64
	id, err := c.WaitForStart(context.Background(), func(s Sender) bool {
		asked = append(asked, s.ChatID)
		return s.ChatID == 444
	})
	if err != nil || id != 444 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if fmt.Sprint(asked) != "[333 444]" {
		t.Fatalf("asked = %v: only fresh private /start senders may be offered", asked)
	}
	if fmt.Sprint(sent) != "[444]" {
		t.Fatalf("replied to %v: only the confirmed chat may get a reply", sent)
	}
	if fmt.Sprint(offsets) != "[-1 11]" {
		t.Fatalf("offsets = %v, want old updates skipped", offsets)
	}
}

func TestSendMessageKBSendsKeyboardAndReturnsID(t *testing.T) {
	var got map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"ok":true,"result":{"message_id":777}}`)
	})
	kb := Keyboard{{{Text: "☐1", Data: "t:1:1"}, {Text: "☐2", Data: "t:1:2"}}}
	id, err := c.SendMessageKB(context.Background(), 5, "x", kb)
	if err != nil || id != 777 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	rm := got["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	btn := rm[0].([]any)[1].(map[string]any)
	if btn["text"] != "☐2" || btn["callback_data"] != "t:1:2" {
		t.Fatalf("keyboard json = %v", got["reply_markup"])
	}
}

func TestEditMessage(t *testing.T) {
	var path string
	var got map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"ok":true,"result":{}}`)
	})
	if err := c.EditMessage(context.Background(), 5, 777, "new", Keyboard{{{Text: "☑1", Data: "t:1:1"}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/editMessageText") || got["message_id"] != float64(777) || got["chat_id"] != float64(5) || got["text"] != "new" {
		t.Fatalf("path=%s body=%v", path, got)
	}
}

func TestEditMessageNotModifiedIsNotAnError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}`)
	})
	if err := c.EditMessage(context.Background(), 5, 777, "same", nil); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestGetUpdatesParsesCallbackQueryAndAsksForIt(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"ok":true,"result":[{"update_id":9,"callback_query":{"id":"abc","data":"t:3:2","from":{"id":42},"message":{"message_id":55,"chat":{"id":42}}}}]}`)
	})
	ups, err := c.GetUpdates(context.Background(), 0, 0)
	if err != nil || len(ups) != 1 {
		t.Fatalf("ups=%+v err=%v", ups, err)
	}
	cq := ups[0].CallbackQuery
	if cq == nil || cq.ID != "abc" || cq.Data != "t:3:2" || cq.Message.MessageID != 55 || cq.Message.Chat.ID != 42 {
		t.Fatalf("callback = %+v", cq)
	}
	if fmt.Sprint(body["allowed_updates"]) != "[message callback_query]" {
		t.Fatalf("allowed_updates = %v", body["allowed_updates"])
	}
}

func TestAnswerCallback(t *testing.T) {
	var path string
	var got map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"ok":true,"result":true}`)
	})
	if err := c.AnswerCallback(context.Background(), "abc", "ok"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/answerCallbackQuery") || got["callback_query_id"] != "abc" || got["text"] != "ok" {
		t.Fatalf("path=%s body=%v", path, got)
	}
}
