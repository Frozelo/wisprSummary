package wispr

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeAuth is a minimal MCP resource + OAuth authorization server.
type fakeAuth struct {
	srv *httptest.Server

	mu        sync.Mutex
	challenge string
	refreshes int
	rejectRT  bool
}

func newFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	f := &fakeAuth{}
	mux := http.NewServeMux()
	mux.HandleFunc("/connect/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.srv.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"authorization_servers":["`+f.srv.URL+`"]}`)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"authorization_endpoint":"`+f.srv.URL+`/authorize","token_endpoint":"`+f.srv.URL+`/token","registration_endpoint":"`+f.srv.URL+`/register"}`)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"client_id":"cid"}`)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "cid" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != f.srv.URL+"/connect/mcp" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.challenge = q.Get("code_challenge")
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=abc&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			f.mu.Lock()
			ok := r.Form.Get("code") == "abc" && base64.RawURLEncoding.EncodeToString(sum[:]) == f.challenge
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			io.WriteString(w, `{"access_token":"at1","refresh_token":"rt1","expires_in":3600}`)
		case "refresh_token":
			f.mu.Lock()
			f.refreshes++
			reject := f.rejectRT
			f.mu.Unlock()
			if reject || r.Form.Get("refresh_token") != "rt1" {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			io.WriteString(w, `{"access_token":"at2","refresh_token":"rt2","expires_in":3600}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestPKCE(t *testing.T) {
	v, c, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(v))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); c != want {
		t.Fatalf("challenge = %q, want %q", c, want)
	}
	if len(v) < 43 {
		t.Fatalf("verifier too short: %d", len(v))
	}
}

func TestLoginAndRefresh(t *testing.T) {
	f := newFakeAuth(t)
	store := TokenStore{Path: filepath.Join(t.TempDir(), "data", "tok.json")}
	hc := f.srv.Client()

	err := Login(context.Background(), LoginOptions{
		MCPURL: f.srv.URL + "/connect/mcp",
		HTTP:   hc,
		Store:  store,
		OpenBrowser: func(u string) error {
			go func() { // the "browser": follows the redirect to our loopback callback
				resp, err := hc.Get(u)
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	tok, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at1" || tok.RefreshToken != "rt1" || tok.ClientID != "cid" {
		t.Fatalf("unexpected token: %+v", tok)
	}
	if fi, _ := os.Stat(store.Path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file perms = %v, want 0600", fi.Mode().Perm())
	}

	// Fresh token is reused without hitting the refresh endpoint.
	ts := NewTokenSource(store, hc)
	if got, err := ts.AccessToken(context.Background()); err != nil || got != "at1" {
		t.Fatalf("AccessToken = %q, %v", got, err)
	}
	if f.refreshes != 0 {
		t.Fatalf("unexpected refresh")
	}

	// Expired token: refresh, persist rotated refresh token.
	ts.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if got, err := ts.AccessToken(context.Background()); err != nil || got != "at2" {
		t.Fatalf("AccessToken after expiry = %q, %v", got, err)
	}
	saved, _ := store.Load()
	if saved.RefreshToken != "rt2" || saved.AccessToken != "at2" {
		t.Fatalf("refreshed token not persisted: %+v", saved)
	}
}

func TestRefreshInvalidGrantRequiresLogin(t *testing.T) {
	f := newFakeAuth(t)
	f.rejectRT = true
	store := TokenStore{Path: filepath.Join(t.TempDir(), "tok.json")}
	if err := store.Save(Token{
		AccessToken: "old", RefreshToken: "rt1", ClientID: "cid",
		TokenEndpoint: f.srv.URL + "/token", Expiry: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := NewTokenSource(store, f.srv.Client()).AccessToken(context.Background())
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v, want ErrLoginRequired", err)
	}
}

func TestMissingTokenFile(t *testing.T) {
	store := TokenStore{Path: filepath.Join(t.TempDir(), "none.json")}
	if _, err := store.Load(); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v, want ErrLoginRequired", err)
	}
}

func TestLoginStateMismatch(t *testing.T) {
	f := newFakeAuth(t)
	hc := f.srv.Client()
	err := Login(context.Background(), LoginOptions{
		MCPURL: f.srv.URL + "/connect/mcp",
		HTTP:   hc,
		Store:  TokenStore{Path: filepath.Join(t.TempDir(), "tok.json")},
		OpenBrowser: func(u string) error {
			pu, _ := url.Parse(u)
			go func() {
				resp, err := hc.Get(pu.Query().Get("redirect_uri") + "?code=abc&state=forged")
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected state mismatch error")
	}
}

func TestMeetingPageSkipsMalformedItems(t *testing.T) {
	var p MeetingPage
	in := `{"meetings":[
		{"id":"a","title":"ok","start":"2026-10-20T09:00:00Z","modified_at":"2026-10-20T10:00:00Z"},
		{"id":"b","title":"bad","start":"","modified_at":"nope"},
		{"id":"c","title":"ok2","start":null,"modified_at":"2026-10-20T11:00:00Z"}],
		"has_more":true,"next_cursor":"xyz"}`
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Meetings) != 2 || p.Skipped != 1 || !p.HasMore || p.NextCursor != "xyz" ||
		p.Meetings[0].ID != "a" || p.Meetings[1].ID != "c" {
		t.Fatalf("page = %+v", p)
	}
}
