package wispr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrLoginRequired means there is no usable OAuth session; the user must run `bot login`.
var ErrLoginRequired = errors.New("wispr: login required (run `bot login`)")

const expirySkew = time.Minute

// Token is the persisted OAuth session. It is never logged.
type Token struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	Expiry        time.Time `json:"expiry"`
	ClientID      string    `json:"client_id"`
	TokenEndpoint string    `json:"token_endpoint"`
	Resource      string    `json:"resource"`
}

// TokenStore persists a Token in a single 0600 file.
type TokenStore struct{ Path string }

func (s TokenStore) Load() (Token, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, ErrLoginRequired
	}
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, fmt.Errorf("wispr: corrupt token file: %w", err)
	}
	return t, nil
}

// Save writes atomically (temp file in the same dir + rename).
func (s TokenStore) Save(t Token) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".token-*") // created with mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.Path)
}

// TokenSource hands out valid access tokens, refreshing and persisting as needed.
type TokenSource struct {
	store TokenStore
	hc    *http.Client
	now   func() time.Time

	mu  sync.Mutex
	tok Token
	ok  bool
}

func NewTokenSource(store TokenStore, hc *http.Client) *TokenSource {
	return &TokenSource{store: store, hc: hc, now: time.Now}
}

func (s *TokenSource) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		t, err := s.store.Load()
		if err != nil {
			return "", err
		}
		s.tok, s.ok = t, true
	}
	if s.now().Add(expirySkew).Before(s.tok.Expiry) {
		return s.tok.AccessToken, nil
	}
	return s.refreshLocked(ctx)
}

// Invalidate forces a refresh on the next AccessToken call (e.g. after a 401).
func (s *TokenSource) Invalidate() {
	s.mu.Lock()
	s.tok.Expiry = time.Time{}
	s.mu.Unlock()
}

func (s *TokenSource) refreshLocked(ctx context.Context) (string, error) {
	if s.tok.RefreshToken == "" {
		return "", ErrLoginRequired
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.tok.RefreshToken},
		"client_id":     {s.tok.ClientID},
	}
	if s.tok.Resource != "" {
		form.Set("resource", s.tok.Resource)
	}
	res, err := tokenRequest(ctx, s.hc, s.tok.TokenEndpoint, form, s.now())
	if err != nil {
		var oe *oauthError
		if errors.As(err, &oe) && oe.Code == "invalid_grant" {
			return "", ErrLoginRequired
		}
		return "", err
	}
	next := s.tok
	next.AccessToken = res.AccessToken
	next.Expiry = res.Expiry
	if res.RefreshToken != "" { // refresh tokens may rotate: always keep the newest
		next.RefreshToken = res.RefreshToken
	}
	if err := s.store.Save(next); err != nil {
		return "", fmt.Errorf("wispr: save refreshed token: %w", err)
	}
	s.tok = next
	return next.AccessToken, nil
}
