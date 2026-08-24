// m365-copilot-proxy
//
// A local translation proxy that lets OpenAI-compatible clients (OpenCode,
// curl, anything speaking /v1/chat/completions) talk to Microsoft 365 Copilot
// through the Microsoft Graph Copilot Chat API.
//
//   OpenCode  --OpenAI JSON-->  :8080/v1/chat/completions  --Graph JSON-->  graph.microsoft.com/beta/copilot
//
// Design notes (read these before changing anything):
//
//   * Auth is delegated. The Copilot Chat API acts *as a licensed user*, so the
//     default flow is OAuth2 Device Code. Client-credentials is wired behind
//     AUTH_MODE=client_credentials purely so you can confirm the 403 yourself.
//   * Refresh tokens are cached at TOKEN_CACHE (0600) so you log in once.
//   * Copilot has no "messages[]" concept per request; it has a conversation
//     with one text message per turn. We flatten the OpenAI history into a single
//     prompt and open a fresh Graph conversation per request. Stateless, boring,
//     correct enough.
//   * Copilot cannot return tool_calls. Any "tools" in the request are ignored.
//   * stream:true is honoured by emitting the finished answer as OpenAI SSE
//     chunks; Graph's own streaming shape is not stable enough to depend on.
//
// Every Graph URL is an env var because the Copilot API is /beta and Microsoft
// moves it. Nothing below is a placeholder — set the env vars and run.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration — all from environment, with sane defaults where one exists.
// ---------------------------------------------------------------------------

type Config struct {
	Listen       string // address for the local server
	TenantID     string // Entra ID tenant (GUID or "organizations")
	ClientID     string // App registration (public client for device code)
	ClientSecret string // only used when AuthMode == "client_credentials"
	AuthMode     string // "device_code" (default) | "client_credentials"
	Scopes       string // space-separated delegated scopes
	TokenCache   string // path of the refresh-token cache file

	GraphBase      string // https://graph.microsoft.com/beta
	ConvPath       string // /copilot/conversations
	ChatPathFmt    string // /copilot/conversations/%s/chat
	ModelName      string // what we advertise on /v1/models and echo back
	TimeZone       string // locationHint.timeZone sent to Copilot
	RequestTimeout time.Duration
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen:       env("LISTEN", "127.0.0.1:8080"),
		TenantID:     env("M365_TENANT_ID", "organizations"),
		ClientID:     os.Getenv("M365_CLIENT_ID"),
		ClientSecret: os.Getenv("M365_CLIENT_SECRET"),
		AuthMode:     env("AUTH_MODE", "device_code"),
		// Delegated permissions the Copilot Chat API expects to be consented.
		// "offline_access" is what gives us a refresh token.
		Scopes: env("M365_SCOPES", "https://graph.microsoft.com/.default offline_access"), // .default = everything already consented on the app; nothing new to request
		TokenCache:     env("TOKEN_CACHE", filepath.Join(home, ".config", "m365-copilot-proxy", "token.json")),
		GraphBase:      env("GRAPH_BASE", "https://graph.microsoft.com/beta"),
		ConvPath:       env("GRAPH_CONV_PATH", "/copilot/conversations"),
		ChatPathFmt:    env("GRAPH_CHAT_PATH_FMT", "/copilot/conversations/%s/chat"),
		ModelName:      env("MODEL_NAME", "m365-copilot"),
		TimeZone:       env("M365_TIMEZONE", "UTC"),
		RequestTimeout: 180 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// Entra ID authentication layer (stdlib OAuth2: device code + refresh + CC)
// ---------------------------------------------------------------------------

// tokenSet is what we persist. The access token is short-lived; the refresh
// token is the thing worth keeping.
type tokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// oauthResp is the shape of every token-endpoint response from Entra ID.
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
}

func NewAuthenticator(cfg Config) *Authenticator {
	a := &Authenticator{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
	a.tok = a.loadCache() // may be nil; that's fine
	return a
}

func (a *Authenticator) tokenURL() string {
	return "https://login.microsoftonline.com/" + a.cfg.TenantID + "/oauth2/v2.0/token"
}
func (a *Authenticator) deviceURL() string {
	return "https://login.microsoftonline.com/" + a.cfg.TenantID + "/oauth2/v2.0/devicecode"
}

// Token returns a valid bearer token, refreshing or (re)authenticating as
// needed. Safe for concurrent use.
func (a *Authenticator) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Fast path: cached, with a 2-minute safety margin.
	if a.tok != nil && time.Until(a.tok.ExpiresAt) > 2*time.Minute {
		return a.tok.AccessToken, nil
	}

	var (
		ts  *tokenSet
		err error
	)
	switch a.cfg.AuthMode {
	case "client_credentials":
		ts, err = a.clientCredentials(ctx)
	default: // device_code
		if a.tok != nil && a.tok.RefreshToken != "" {
			ts, err = a.refresh(ctx, a.tok.RefreshToken)
			if err != nil {
				log.Printf("auth: refresh failed (%v); falling back to device code", err)
				ts, err = a.deviceCode(ctx)
			}
		} else {
			ts, err = a.deviceCode(ctx)
		}
	}
	if err != nil {
		return "", err
	}
	a.tok = ts
	a.saveCache(ts)
	return ts.AccessToken, nil
}

// postForm hits the token endpoint and decodes the standard response.
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

// clientCredentials — app-only token. Included for completeness; the Copilot
// Chat API will answer 403 because it requires a signed-in licensed user.
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

// refresh exchanges a refresh token for a new access token.
func (a *Authenticator) refresh(ctx context.Context, rt string) (*tokenSet, error) {
	r, err := a.postForm(ctx, a.tokenURL(), url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {a.cfg.ClientID},
		"refresh_token": {rt},
		"scope":         {a.cfg.Scopes},
	})
	if err != nil {
		return nil, err
	}
	if r.Error != "" {
		return nil, fmt.Errorf("refresh: %s: %s", r.Error, r.ErrorDesc)
	}
	if r.RefreshToken == "" { // Entra usually rotates; keep the old one if not
		r.RefreshToken = rt
	}
	return toTokenSet(r), nil
}

// deviceCode runs the OAuth2 device authorization grant: prints a URL + code
// to the proxy's stderr, then polls until the user finishes in a browser.
func (a *Authenticator) deviceCode(ctx context.Context) (*tokenSet, error) {
	if a.cfg.ClientID == "" {
		return nil, errors.New("M365_CLIENT_ID is required")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.deviceURL(),
		strings.NewReader(url.Values{
			"client_id": {a.cfg.ClientID},
			"scope":     {a.cfg.Scopes},
		}.Encode()))
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

	interval := time.Duration(max(dc.Interval, 5)) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		r, err := a.postForm(ctx, a.tokenURL(), url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {a.cfg.ClientID},
			"device_code": {dc.DeviceCode},
		})
		if err != nil {
			return nil, err
		}
		switch r.Error {
		case "":
			fmt.Fprintln(os.Stderr, "=== Signed in. Token cached. ===")
			return toTokenSet(r), nil
		case "authorization_pending":
			continue
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
		return // nothing durable to keep
	}
	_ = os.MkdirAll(filepath.Dir(a.cfg.TokenCache), 0o700)
	b, _ := json.MarshalIndent(ts, "", "  ")
	if err := os.WriteFile(a.cfg.TokenCache, b, 0o600); err != nil {
		log.Printf("auth: could not write token cache: %v", err)
	}
}

// ---------------------------------------------------------------------------
// OpenAI wire types (only the fields OpenCode actually uses)
// ---------------------------------------------------------------------------

// oaiContent handles both `"content": "text"` and the array-of-parts form
// that the Vercel AI SDK (which OpenCode uses) sends.
type oaiContent string

func (c *oaiContent) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = oaiContent(s)
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	*c = oaiContent(sb.String())
	return nil
}

type oaiMessage struct {
	Role    string     `json:"role"`
	Content oaiContent `json:"content"`
	Name    string     `json:"name,omitempty"`
}

type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Stream   bool         `json:"stream"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiChoice struct {
	Index        int         `json:"index"`
	Message      *oaiMessage `json:"message,omitempty"`
	Delta        *oaiMessage `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

type oaiResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// Microsoft Graph Copilot Chat API wire types
// ---------------------------------------------------------------------------

type graphChatRequest struct {
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
	LocationHint struct {
		TimeZone string `json:"timeZone"`
	} `json:"locationHint"`
	// contexts lets us pass the system prompt as grounding rather than as
	// chat text, which Copilot honours better.
	Contexts []graphContext `json:"contexts,omitempty"`
}

type graphContext struct {
	Description string `json:"description"`
	Text        string `json:"text"`
}

type graphMessage struct {
	ID           string `json:"id"`
	Text         string `json:"text"`
	Attributions []struct {
		ProviderDisplayName string `json:"providerDisplayName"`
		SeeMoreWebURL       string `json:"seeMoreWebUrl"`
	} `json:"attributions"`
}

type graphChatResponse struct {
	ID       string         `json:"id"`
	Messages []graphMessage `json:"messages"`
	// Single-message variant some builds return.
	Text  string `json:"text"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Payload mapping: OpenAI messages[] -> one Copilot prompt (+ contexts)
// ---------------------------------------------------------------------------

// flatten turns the OpenAI conversation into (systemContext, prompt).
// Prior turns are rendered as a transcript so Copilot sees the history; the
// final user message is the actual question.
func flatten(msgs []oaiMessage) (system string, prompt string) {
	var sys, hist []string
	last := -1
	for i, m := range msgs {
		if m.Role == "user" {
			last = i
		}
	}
	for i, m := range msgs {
		text := strings.TrimSpace(string(m.Content))
		if text == "" {
			continue
		}
		switch m.Role {
		case "system", "developer":
			sys = append(sys, text)
		case "user":
			if i == last {
				continue // goes in the prompt proper
			}
			hist = append(hist, "User: "+text)
		case "assistant":
			hist = append(hist, "Assistant: "+text)
		case "tool":
			hist = append(hist, "Tool result: "+text)
		}
	}
	var sb strings.Builder
	if len(hist) > 0 {
		sb.WriteString("Conversation so far:\n")
		sb.WriteString(strings.Join(hist, "\n\n"))
		sb.WriteString("\n\nContinue the conversation. ")
	}
	if last >= 0 {
		sb.WriteString(strings.TrimSpace(string(msgs[last].Content)))
	}
	return strings.Join(sys, "\n\n"), sb.String()
}

func buildGraphRequest(cfg Config, system, prompt string) graphChatRequest {
	var gr graphChatRequest
	gr.Message.Text = prompt
	gr.LocationHint.TimeZone = cfg.TimeZone
	if system != "" {
		gr.Contexts = []graphContext{{
			Description: "System instructions from the calling application. Follow them.",
			Text:        system,
		}}
	}
	return gr
}

// ---------------------------------------------------------------------------
// Graph client
// ---------------------------------------------------------------------------

type GraphClient struct {
	cfg  Config
	auth *Authenticator
	http *http.Client
}

func (g *GraphClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	tok, err := g.auth.Token(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("auth: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.cfg.GraphBase+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

// Chat opens a conversation and sends one message, returning Copilot's text.
func (g *GraphClient) Chat(ctx context.Context, gr graphChatRequest) (string, error) {
	// 1. Create the conversation.
	b, status, err := g.do(ctx, http.MethodPost, g.cfg.ConvPath, map[string]any{})
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", fmt.Errorf("graph create conversation: HTTP %d: %s", status, truncate(b, 600))
	}
	var conv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &conv); err != nil || conv.ID == "" {
		return "", fmt.Errorf("graph create conversation: no id in %s", truncate(b, 300))
	}

	// 2. Send the message.
	b, status, err = g.do(ctx, http.MethodPost, fmt.Sprintf(g.cfg.ChatPathFmt, url.PathEscape(conv.ID)), gr)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", fmt.Errorf("graph chat: HTTP %d: %s", status, truncate(b, 600))
	}
	var out graphChatResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("graph chat: decode: %w (%s)", err, truncate(b, 300))
	}
	if out.Error != nil {
		return "", fmt.Errorf("graph chat: %s: %s", out.Error.Code, out.Error.Message)
	}

	// 3. Extract the assistant text. Copilot returns the full exchange; the
	//    assistant's reply is the last message.
	var text string
	if n := len(out.Messages); n > 0 {
		m := out.Messages[n-1]
		text = m.Text
		if len(m.Attributions) > 0 {
			var refs []string
			for _, a := range m.Attributions {
				if a.SeeMoreWebURL != "" {
					refs = append(refs, fmt.Sprintf("- %s: %s", a.ProviderDisplayName, a.SeeMoreWebURL))
				}
			}
			if len(refs) > 0 {
				text += "\n\nSources:\n" + strings.Join(refs, "\n")
			}
		}
	} else {
		text = out.Text
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("graph chat: empty reply in %s", truncate(b, 300))
	}
	return text, nil
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// HTTP server: OpenAI-compatible surface
// ---------------------------------------------------------------------------

type Server struct {
	cfg   Config
	graph *GraphClient
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return logMiddleware(mux)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// handleModels — OpenCode/AI-SDK sometimes lists models before chatting.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id": s.cfg.ModelName, "object": "model", "created": time.Now().Unix(), "owned_by": "microsoft",
		}},
	})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req oaiRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "messages[] is empty")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	system, prompt := flatten(req.Messages)
	answer, err := s.graph.Chat(ctx, buildGraphRequest(s.cfg, system, prompt))
	if err != nil {
		log.Printf("chat: %v", err)
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}

	model := req.Model
	if model == "" {
		model = s.cfg.ModelName
	}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	usage := &oaiUsage{
		PromptTokens:     approxTokens(system + prompt),
		CompletionTokens: approxTokens(answer),
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	if req.Stream {
		s.writeStream(w, id, model, answer, usage)
		return
	}
	stop := "stop"
	writeJSON(w, http.StatusOK, oaiResponse{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []oaiChoice{{Index: 0, Message: &oaiMessage{Role: "assistant", Content: oaiContent(answer)}, FinishReason: &stop}},
		Usage:   usage,
	})
}

// writeStream emits the answer as OpenAI-style SSE so `stream:true` clients
// get exactly what they expect: role delta, content deltas, finish, [DONE].
func (s *Server) writeStream(w http.ResponseWriter, id, model, answer string, usage *oaiUsage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriter(w)

	emit := func(c oaiResponse) {
		b, _ := json.Marshal(c)
		bw.WriteString("data: ")
		bw.Write(b)
		bw.WriteString("\n\n")
		bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta *oaiMessage, finish *string) oaiResponse {
		return oaiResponse{
			ID: id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: model,
			Choices: []oaiChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
	}

	emit(chunk(&oaiMessage{Role: "assistant", Content: ""}, nil))
	// Chunk on word boundaries so the TUI renders progressively.
	for _, piece := range splitChunks(answer, 24) {
		emit(chunk(&oaiMessage{Content: oaiContent(piece)}, nil))
	}
	stop := "stop"
	final := chunk(&oaiMessage{}, &stop)
	final.Usage = usage
	emit(final)
	bw.WriteString("data: [DONE]\n\n")
	bw.Flush()
	if flusher != nil {
		flusher.Flush()
	}
}

// splitChunks breaks text into ~n-rune pieces without splitting words.
func splitChunks(s string, n int) []string {
	var out []string
	var cur strings.Builder
	for _, word := range strings.SplitAfter(s, " ") {
		cur.WriteString(word)
		if cur.Len() >= n {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// approxTokens is a cheap estimate; Graph reports no token counts.
func approxTokens(s string) int { return (len(s) + 3) / 4 }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOpenAIError uses the OpenAI error envelope so clients surface the
// message instead of choking on an unknown shape.
func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "proxy_error", "code": status},
	})
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	cfg := loadConfig()
	if cfg.ClientID == "" {
		log.Fatal("M365_CLIENT_ID is required (app registration client id)")
	}
	auth := NewAuthenticator(cfg)
	srv := &Server{
		cfg:   cfg,
		graph: &GraphClient{cfg: cfg, auth: auth, http: &http.Client{Timeout: cfg.RequestTimeout}},
	}

	// Warm the token at startup so the device-code prompt appears now, not in
	// the middle of OpenCode's first request.
	if _, err := auth.Token(context.Background()); err != nil {
		log.Fatalf("initial auth failed: %v", err)
	}

	log.Printf("m365-copilot-proxy listening on http://%s  (model=%s, auth=%s)", cfg.Listen, cfg.ModelName, cfg.AuthMode)
	log.Fatal(http.ListenAndServe(cfg.Listen, srv.routes()))
}
