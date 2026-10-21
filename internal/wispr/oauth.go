package wispr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	scope        = "openid offline_access"
	clientName   = "wispr-telegram-assistant"
	loginTimeout = 5 * time.Minute
	maxBody      = 1 << 20
)

// Metadata is the subset of RFC 8414 authorization server metadata we use.
type Metadata struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

var resourceMetadataRe = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// Discover follows the MCP authorization flow: 401 challenge -> protected
// resource metadata (RFC 9728) -> authorization server metadata (RFC 8414).
func Discover(ctx context.Context, hc *http.Client, mcpURL string) (Metadata, error) {
	prmURL, err := protectedResourceURL(ctx, hc, mcpURL)
	if err != nil {
		return Metadata{}, err
	}
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, hc, prmURL, &prm); err != nil {
		return Metadata{}, fmt.Errorf("protected resource metadata: %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return Metadata{}, errors.New("protected resource metadata lists no authorization server")
	}
	issuer := strings.TrimRight(prm.AuthorizationServers[0], "/")
	var md Metadata
	if err := getJSON(ctx, hc, issuer+"/.well-known/oauth-authorization-server", &md); err != nil {
		return Metadata{}, fmt.Errorf("authorization server metadata: %w", err)
	}
	if md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" || md.RegistrationEndpoint == "" {
		return Metadata{}, errors.New("authorization server metadata is missing required endpoints")
	}
	return md, nil
}

func protectedResourceURL(ctx context.Context, hc *http.Client, mcpURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if m := resourceMetadataRe.FindStringSubmatch(resp.Header.Get("WWW-Authenticate")); m != nil {
		return m[1], nil
	}
	u, err := url.Parse(mcpURL)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource", nil
}

// Register performs dynamic client registration for a public (PKCE) client.
func Register(ctx context.Context, hc *http.Client, md Metadata, redirectURI string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      scope,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("client registration failed: %s: %s", resp.Status, snippet(b))
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ClientID == "" {
		return "", errors.New("client registration returned no client_id")
	}
	return out.ClientID, nil
}

func newPKCE() (verifier, challenge string, err error) {
	verifier, err = randomString(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// LoginOptions configures the interactive browser login.
type LoginOptions struct {
	MCPURL      string
	HTTP        *http.Client
	Store       TokenStore
	OpenBrowser func(url string) error // nil: DefaultOpenBrowser
	Out         io.Writer              // where the authorization URL is printed
}

// Login runs Authorization Code + PKCE with a loopback redirect and stores the tokens.
func Login(ctx context.Context, o LoginOptions) error {
	if o.OpenBrowser == nil {
		o.OpenBrowser = DefaultOpenBrowser
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	md, err := Discover(ctx, o.HTTP, o.MCPURL)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	redirectURI := fmt.Sprintf("http://%s/callback", ln.Addr())

	clientID, err := Register(ctx, o.HTTP, md, redirectURI)
	if err != nil {
		ln.Close()
		return err
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		ln.Close()
		return err
	}
	state, err := randomString(16)
	if err != nil {
		ln.Close()
		return err
	}

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var res result
		switch {
		case q.Get("state") != state:
			res.err = errors.New("state mismatch")
		case q.Get("error") != "":
			res.err = fmt.Errorf("authorization error: %s", q.Get("error"))
		case q.Get("code") == "":
			res.err = errors.New("callback without code")
		default:
			res.code = q.Get("code")
		}
		if res.err != nil {
			http.Error(w, "Login failed. You can close this tab.", http.StatusBadRequest)
		} else {
			fmt.Fprintln(w, "Login complete. You can close this tab.")
		}
		select {
		case resCh <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := md.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {scope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {o.MCPURL},
	}.Encode()

	if o.Out != nil {
		fmt.Fprintf(o.Out, "Open this URL to authorize Wispr Flow access (read-only):\n\n%s\n\n", authURL)
	}
	_ = o.OpenBrowser(authURL) // best effort; the URL is printed above

	var code string
	select {
	case r := <-resCh:
		if r.err != nil {
			return r.err
		}
		code = r.code
	case <-ctx.Done():
		return fmt.Errorf("login not completed: %w", ctx.Err())
	}

	tr, err := tokenRequest(ctx, o.HTTP, md.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {o.MCPURL},
	}, time.Now())
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	if tr.RefreshToken == "" {
		return errors.New("no refresh token issued (offline_access not granted); the service cannot run unattended")
	}
	return o.Store.Save(Token{
		AccessToken:   tr.AccessToken,
		RefreshToken:  tr.RefreshToken,
		Expiry:        tr.Expiry,
		ClientID:      clientID,
		TokenEndpoint: md.TokenEndpoint,
		Resource:      o.MCPURL,
	})
}

type tokenResult struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

type oauthError struct {
	Code string
	Desc string
}

func (e *oauthError) Error() string { return "oauth error: " + e.Code + " " + e.Desc }

func tokenRequest(ctx context.Context, hc *http.Client, endpoint string, form url.Values, now time.Time) (tokenResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return tokenResult{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return tokenResult{}, &oauthError{Code: e.Error, Desc: e.Desc}
		}
		return tokenResult{}, fmt.Errorf("token endpoint: %s", resp.Status)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.AccessToken == "" {
		return tokenResult{}, errors.New("token endpoint returned no access_token")
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600 // not specified by the server; refresh conservatively
	}
	return tokenResult{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		Expiry:       now.Add(time.Duration(out.ExpiresIn) * time.Second),
	}, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v)
}

func snippet(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return strings.TrimSpace(string(b))
}

// DefaultOpenBrowser opens u in the system browser.
func DefaultOpenBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
