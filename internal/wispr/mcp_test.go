package wispr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func newFakeMCP(t *testing.T, reject401Once bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var rejected, refreshes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		io.WriteString(w, `{"access_token":"fresh","expires_in":3600}`)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" || (reject401Once && auth == "Bearer stale" && rejected.CompareAndSwap(0, 1)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "initialize" && req.Method != "notifications/initialized" &&
			r.Header.Get("Mcp-Session-Id") != "sess-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`, *req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list": // answered as an SSE stream with a preceding notification
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"search_meetings\"}]}}\n\n", *req.ID)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"hi"}]}}`, *req.ID)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &refreshes
}

func newTestClient(t *testing.T, srv *httptest.Server, access string) *Client {
	t.Helper()
	store := TokenStore{Path: filepath.Join(t.TempDir(), "tok.json")}
	if err := store.Save(Token{
		AccessToken: access, RefreshToken: "rt", ClientID: "cid",
		TokenEndpoint: srv.URL + "/token", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return NewClient(srv.URL+"/mcp", srv.Client(), NewTokenSource(store, srv.Client()))
}

func TestClientListAndCall(t *testing.T) {
	srv, _ := newFakeMCP(t, false)
	c := newTestClient(t, srv, "good")

	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "search_meetings" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	res, err := c.CallTool(context.Background(), "search_meetings", nil)
	if err != nil || res.Text() != "hi" {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
}

func TestClientRefreshesOn401(t *testing.T) {
	srv, refreshes := newFakeMCP(t, true)
	c := newTestClient(t, srv, "stale")

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes.Load())
	}
}
