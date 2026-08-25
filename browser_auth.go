package main

// AUTH_MODE=browser — OAuth2 authorization-code flow with PKCE through a
// localhost redirect. Conditional Access policies that block device code
// almost always allow this: it is the same flow every desktop app uses.
//
// App registration needs a redirect URI of
//   http://localhost:8080/auth/callback   (platform: "Mobile and desktop
//   applications" for public clients, or "Web" for confidential ones)
// adjust the port to LISTEN.
//
// Flow: launcher (or user) opens GET /auth/login → 302 to Microsoft → user
// signs in → Microsoft redirects to /auth/callback?code=… → we exchange the
// code (+ PKCE verifier, + client_secret if confidential) → token cached.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type browserAuth struct {
	mu       sync.Mutex
	state    string
	verifier string
	started  time.Time
}

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *Authenticator) redirectURI() string {
	return "http://localhost:" + a.cfg.listenPort() + "/auth/callback"
}

// loginHandler starts the flow.
func (a *Authenticator) loginHandler(w http.ResponseWriter, r *http.Request) {
	a.bmu.Lock()
	a.browser = &browserAuth{state: randB64(24), verifier: randB64(48), started: time.Now()}
	b := a.browser
	a.bmu.Unlock()

	sum := sha256.Sum256([]byte(b.verifier))
	q := url.Values{
		"client_id":             {a.cfg.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {a.redirectURI()},
		"response_mode":         {"query"},
		"scope":                 {a.cfg.Scopes},
		"state":                 {b.state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	http.Redirect(w, r, "https://login.microsoftonline.com/"+a.cfg.TenantID+"/oauth2/v2.0/authorize?"+q.Encode(), http.StatusFound)
}

// callbackHandler finishes it.
func (a *Authenticator) callbackHandler(w http.ResponseWriter, r *http.Request) {
	a.bmu.Lock()
	b := a.browser
	a.bmu.Unlock()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		a.setErr(e + ": " + q.Get("error_description"))
		http.Error(w, "Microsoft returned: "+e+"\n"+q.Get("error_description"), 400)
		return
	}
	if b == nil || q.Get("state") != b.state || time.Since(b.started) > 10*time.Minute {
		http.Error(w, "stale or mismatched sign-in attempt — open /auth/login again", 400)
		return
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {a.cfg.ClientID},
		"code":          {q.Get("code")},
		"redirect_uri":  {a.redirectURI()},
		"code_verifier": {b.verifier},
		"scope":         {a.cfg.Scopes},
	}
	if a.cfg.ClientSecret != "" {
		form.Set("client_secret", a.cfg.ClientSecret)
	}
	resp, err := a.postForm(r.Context(), a.tokenURL(), form)
	if err != nil {
		a.setErr(err.Error())
		http.Error(w, err.Error(), 502)
		return
	}
	if resp.Error != "" {
		a.setErr(resp.Error + ": " + resp.ErrorDesc)
		http.Error(w, resp.Error+"\n"+resp.ErrorDesc, 400)
		return
	}
	ts := toTokenSet(resp)
	a.mu.Lock()
	a.tok = ts
	a.mu.Unlock()
	a.saveCache(ts)
	a.setErr("")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset=utf-8><title>Signed in</title>
<body style="font-family:system-ui;padding:3rem"><h2>Signed in.</h2><p>Token cached. You can close this tab and go back to the terminal.</p>`)
}

// Token() path for browser mode when nothing is cached: don't block, tell
// the caller where to go.
func (a *Authenticator) browserPending(ctx context.Context) (*tokenSet, error) {
	a.setPendingBrowser()
	return nil, fmt.Errorf("sign in required: open http://localhost:%s/auth/login in a browser", a.cfg.listenPort())
}

// seedHandler — POST {"refresh_token": "..."}: bootstrap from a refresh token
// obtained elsewhere (the CAP workaround from the first field test). The
// token is stored with an expired access token so the next call refreshes.
func (a *Authenticator) seedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST {\"refresh_token\":\"…\"}", 405)
		return
	}
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeJSON(r, &in); err != nil || in.RefreshToken == "" {
		http.Error(w, "need refresh_token", 400)
		return
	}
	ts, err := a.refresh(r.Context(), in.RefreshToken)
	if err != nil {
		http.Error(w, "refresh with seeded token failed: "+err.Error(), 400)
		return
	}
	a.mu.Lock()
	a.tok = ts
	a.mu.Unlock()
	a.saveCache(ts)
	a.setErr("")
	writeJSON(w, 200, map[string]any{"authenticated": true, "expires_at": ts.ExpiresAt})
}
