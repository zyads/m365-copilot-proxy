package main

// Entra ID authentication layer — stdlib OAuth2: device code, refresh, and
// client credentials. Device code is the one that works for the Copilot Chat
// API (delegated-only); client credentials is here so you can see the 403.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type tokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type oauthResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type Authenticator struct {
	cfg  Config
	http *http.Client
	mu   sync.Mutex
	tok  *tokenSet

	// Device-code state for headless runs (service with no terminal): the
	// launcher polls /auth and shows the user the code.
	pmu     sync.Mutex
	pending *pendingAuth
	lastErr string

	bmu     sync.Mutex
	browser *browserAuth
}

func (a *Authenticator) setErr(s string) {
	a.pmu.Lock()
	a.lastErr = s
	a.pmu.Unlock()
}

func (a *Authenticator) setPendingBrowser() {
	a.pmu.Lock()
	a.pending = &pendingAuth{
		Message:         "To sign in, open http://localhost:" + a.cfg.listenPort() + "/auth/login in your browser.",
		VerificationURI: "http://localhost:" + a.cfg.listenPort() + "/auth/login",
		ExpiresAt:       time.Now().Add(24 * time.Hour),
	}
	a.pmu.Unlock()
}

type pendingAuth struct {
	Message         string `json:"message"`
	VerificationURI string `json:"verification_uri"`
	UserCode        string `json:"user_code"`
	ExpiresAt       time.Time
}

// Status is what /auth returns.
func (a *Authenticator) Status() map[string]any {
	a.mu.Lock()
	ok := a.tok != nil && time.Until(a.tok.ExpiresAt) > 0
	hasRT := a.tok != nil && a.tok.RefreshToken != ""
	a.mu.Unlock()
	a.pmu.Lock()
	defer a.pmu.Unlock()
	out := map[string]any{"authenticated": ok || hasRT, "mode": a.cfg.AuthMode}
	if ok || hasRT {
		a.pending = nil
	}
	if a.pending != nil && time.Now().Before(a.pending.ExpiresAt) {
		out["pending"] = a.pending
	}
	if a.lastErr != "" {
		out["last_error"] = a.lastErr
	}
	return out
}

// EnsureAsync kicks off sign-in in the background if there is no usable
// token, so a headless service can start and expose the code via /auth.
func (a *Authenticator) EnsureAsync() {
	go func() {
		if _, err := a.Token(context.Background()); err != nil {
			a.pmu.Lock()
			a.lastErr = err.Error()
			a.pmu.Unlock()
			log.Printf("auth: %v", err)
		}
	}()
}

func NewAuthenticator(cfg Config) *Authenticator {
	a := &Authenticator{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
	a.tok = a.loadCache()
	return a
}

func (a *Authenticator) tokenURL() string {
	return "https://login.microsoftonline.com/" + a.cfg.TenantID + "/oauth2/v2.0/token"
}
func (a *Authenticator) deviceURL() string {
	return "https://login.microsoftonline.com/" + a.cfg.TenantID + "/oauth2/v2.0/devicecode"
}

// Token returns a valid bearer token, refreshing or re-authenticating as needed.
func (a *Authenticator) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok != nil && time.Until(a.tok.ExpiresAt) > 2*time.Minute {
		return a.tok.AccessToken, nil
	}
	var (
		ts  *tokenSet
		err error
	)
	interactive := a.deviceCode
	if a.cfg.AuthMode == "browser" {
		interactive = a.browserPending
	}
	switch a.cfg.AuthMode {
	case "client_credentials":
		ts, err = a.clientCredentials(ctx)
	default:
		if a.tok != nil && a.tok.RefreshToken != "" {
			ts, err = a.refresh(ctx, a.tok.RefreshToken)
			if err != nil {
				log.Printf("auth: refresh failed (%v); interactive sign-in needed", err)
				ts, err = interactive(ctx)
			}
		} else {
			ts, err = interactive(ctx)
		}
	}
	if err != nil {
		return "", err
	}
	a.tok = ts
	a.saveCache(ts)
	return ts.AccessToken, nil
}

func (a *Authenticator) postForm(ctx context.Context, endpoint string, form url.Values) (*oauthResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out oauthResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &out, nil
}

func toTokenSet(r *oauthResp) *tokenSet {
	return &tokenSet{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}
}

func (a *Authenticator) clientCredentials(ctx context.Context) (*tokenSet, error) {
	if a.cfg.ClientSecret == "" {
		return nil, errors.New("AUTH_MODE=client_credentials requires M365_CLIENT_SECRET")
	}
	r, err := a.postForm(ctx, a.tokenURL(), url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.cfg.ClientID},
		"client_secret": {a.cfg.ClientSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
	})
	if err != nil {
		return nil, err
	}
	if r.Error != "" {
		return nil, fmt.Errorf("client_credentials: %s: %s", r.Error, r.ErrorDesc)
	}
	return toTokenSet(r), nil
}

// refresh exchanges a refresh token for a new access token. Confidential app
// registrations require the secret on every token call, refreshes included.
func (a *Authenticator) refresh(ctx context.Context, rt string) (*tokenSet, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {a.cfg.ClientID},
		"refresh_token": {rt},
		"scope":         {a.cfg.Scopes},
	}
	if a.cfg.ClientSecret != "" {
		form.Set("client_secret", a.cfg.ClientSecret)
	}
	r, err := a.postForm(ctx, a.tokenURL(), form)
	if err != nil {
		return nil, err
	}
	if r.Error != "" {
		return nil, fmt.Errorf("refresh: %s: %s", r.Error, r.ErrorDesc)
	}
	if r.RefreshToken == "" {
		r.RefreshToken = rt
	}
	return toTokenSet(r), nil
}

// deviceCode runs the OAuth2 device authorization grant.
func (a *Authenticator) deviceCode(ctx context.Context) (*tokenSet, error) {
	if a.cfg.ClientID == "" {
		return nil, errors.New("M365_CLIENT_ID is required")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.deviceURL(),
		strings.NewReader(url.Values{"client_id": {a.cfg.ClientID}, "scope": {a.cfg.Scopes}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Message         string `json:"message"`
		Error           string `json:"error"`
		ErrorDesc       string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, err
	}
	if dc.Error != "" {
		return nil, fmt.Errorf("devicecode: %s: %s", dc.Error, dc.ErrorDesc)
	}
	fmt.Fprintf(os.Stderr, "\n=== Microsoft sign-in required ===\n%s\n(open %s and enter code %s)\n\n",
		dc.Message, dc.VerificationURI, dc.UserCode)
	a.pmu.Lock()
	a.pending = &pendingAuth{Message: dc.Message, VerificationURI: dc.VerificationURI, UserCode: dc.UserCode,
		ExpiresAt: time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)}
	a.lastErr = ""
	a.pmu.Unlock()
	defer func() { a.pmu.Lock(); a.pending = nil; a.pmu.Unlock() }()

	interval := time.Duration(max(dc.Interval, 5)) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {a.cfg.ClientID},
			"device_code": {dc.DeviceCode},
		}
		if a.cfg.ClientSecret != "" {
			form.Set("client_secret", a.cfg.ClientSecret)
		}
		r, err := a.postForm(ctx, a.tokenURL(), form)
		if err != nil {
			return nil, err
		}
		switch r.Error {
		case "":
			fmt.Fprintln(os.Stderr, "=== Signed in. Token cached. ===")
			return toTokenSet(r), nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return nil, fmt.Errorf("devicecode poll: %s: %s", r.Error, r.ErrorDesc)
		}
	}
	return nil, errors.New("device code expired before sign-in completed")
}

func (a *Authenticator) loadCache() *tokenSet {
	b, err := os.ReadFile(a.cfg.TokenCache)
	if err != nil {
		return nil
	}
	var ts tokenSet
	if json.Unmarshal(b, &ts) != nil {
		return nil
	}
	return &ts
}

func (a *Authenticator) saveCache(ts *tokenSet) {
	if a.cfg.AuthMode == "client_credentials" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(a.cfg.TokenCache), 0o700)
	b, _ := json.MarshalIndent(ts, "", "  ")
	if err := os.WriteFile(a.cfg.TokenCache, b, 0o600); err != nil {
		log.Printf("auth: could not write token cache: %v", err)
	}
}
