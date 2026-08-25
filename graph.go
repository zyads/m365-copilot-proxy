package main

// Microsoft Graph Copilot Chat API client with retry/backoff and a startup
// capability probe.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type graphContext struct {
	Description string `json:"description"`
	Text        string `json:"text"`
}

type graphMessage struct {
	ID           string `json:"id"`
	Type         string `json:"@odata.type"` // #microsoft.graph.copilotUserMessage | copilotResponseMessage …
	Role         string `json:"role"`        // some builds: "user" | "assistant"
	Text         string `json:"text"`
	Attributions []struct {
		ProviderDisplayName string `json:"providerDisplayName"`
		SeeMoreWebURL       string `json:"seeMoreWebUrl"`
	} `json:"attributions"`
}

type graphChatResponse struct {
	ID       string         `json:"id"`
	Messages []graphMessage `json:"messages"`
	Text     string         `json:"text"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type GraphClient struct {
	cfg  Config
	auth *Authenticator
	http *http.Client
}

// chatBody builds the Graph request body. ExtraBody (env GRAPH_EXTRA_BODY) is
// merged last so any future knob Microsoft adds — e.g. a model selector — is
// one env var away with no code change.
func (g *GraphClient) chatBody(prompt string, contexts []graphContext) map[string]any {
	body := map[string]any{
		"message":      map[string]any{"text": prompt},
		"locationHint": map[string]any{"timeZone": g.cfg.TimeZone},
	}
	if len(contexts) > 0 {
		body["contexts"] = contexts
	}
	if g.cfg.ExtraBody != "" {
		var extra map[string]any
		if err := json.Unmarshal([]byte(g.cfg.ExtraBody), &extra); err != nil {
			log.Printf("GRAPH_EXTRA_BODY is not valid JSON, ignoring: %v", err)
		} else {
			for k, v := range extra {
				body[k] = v
			}
		}
	}
	return body
}

// do performs one Graph call with bearer auth and retry on 429/5xx.
func (g *GraphClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	var lastErr error
	for attempt := 0; attempt <= g.cfg.MaxRetries; attempt++ {
		tok, err := g.auth.Token(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("auth: %w", err)
		}
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, g.cfg.GraphBase+path, rdr)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := g.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 429 && resp.StatusCode < 500 {
				return b, resp.StatusCode, nil
			}
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(b, 300))
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if s, err := strconv.Atoi(ra); err == nil {
					sleepCtx(ctx, time.Duration(s)*time.Second)
					continue
				}
			}
		}
		sleepCtx(ctx, time.Duration(1<<attempt)*time.Second)
	}
	return nil, 0, fmt.Errorf("graph %s %s: giving up after %d attempts: %w", method, path, g.cfg.MaxRetries+1, lastErr)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// NewConversation opens a Graph Copilot conversation and returns its id.
func (g *GraphClient) NewConversation(ctx context.Context) (string, error) {
	b, status, err := g.do(ctx, http.MethodPost, g.cfg.ConvPath, map[string]any{})
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		if status == 403 {
			if m := requiredScopesRe.FindSubmatch(b); m != nil {
				return "", fmt.Errorf("Copilot Chat API refused: the app registration must have these DELEGATED Graph permissions admin-consented (all of them, even for plain chat): %s — then sign in again to get a token that carries them", string(m[1]))
			}
			return "", fmt.Errorf("graph create conversation: HTTP 403 (missing scopes or no Copilot license for this user): %s", truncate(b, 600))
		}
		return "", fmt.Errorf("graph create conversation: HTTP %d: %s", status, truncate(b, 600))
	}
	var conv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &conv); err != nil || conv.ID == "" {
		return "", fmt.Errorf("graph create conversation: no id in %s", truncate(b, 300))
	}
	return conv.ID, nil
}

// Send posts one message to a conversation and returns Copilot's reply text
// and the attributions rendered as a "Sources:" footer (empty if none).
func (g *GraphClient) Send(ctx context.Context, convID, prompt string, contexts []graphContext) (text, sources string, err error) {
	path := fmt.Sprintf(g.cfg.ChatPathFmt, url.PathEscape(convID))
	b, status, err := g.do(ctx, http.MethodPost, path, g.chatBody(prompt, contexts))
	if err != nil {
		return "", "", err
	}
	if status/100 != 2 {
		if isTooLarge(status, b) {
			return "", "", errTooLarge
		}
		return "", "", fmt.Errorf("graph chat: HTTP %d: %s", status, truncate(b, 600))
	}
	var out graphChatResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return "", "", fmt.Errorf("graph chat: decode: %w (%s)", err, truncate(b, 300))
	}
	if out.Error != nil {
		return "", "", fmt.Errorf("graph chat: %s: %s", out.Error.Code, out.Error.Message)
	}
	if m := pickResponse(out.Messages, prompt); m != nil {
		text = m.Text
		var refs []string
		for _, a := range m.Attributions {
			if a.SeeMoreWebURL != "" {
				refs = append(refs, fmt.Sprintf("- %s: %s", a.ProviderDisplayName, a.SeeMoreWebURL))
			}
		}
		if len(refs) > 0 {
			sources = "\n\nSources:\n" + strings.Join(refs, "\n")
		}
	} else {
		text = out.Text
	}
	text = stripPolicyNotice(text)
	if strings.TrimSpace(text) == "" {
		return "", "", errNoAnswer
	}
	return text, sources, nil
}

// errNoAnswer: Copilot returned no assistant message (or only a policy
// notice). The server turns this into a nudge rather than a 502.
var errNoAnswer = errors.New("graph chat: Copilot returned no answer")

// pickResponse finds the assistant's message. Field-tested: taking the LAST
// message echoed our own prompt back when Copilot produced no answer.
// Order: @odata.type/role says response → last message that is not the
// prompt we sent → nil.
func pickResponse(msgs []graphMessage, prompt string) *graphMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		t := strings.ToLower(m.Type)
		if strings.Contains(t, "response") || strings.Contains(t, "assistant") || strings.EqualFold(m.Role, "assistant") {
			return &msgs[i]
		}
	}
	// No typed messages: anything whose text isn't (a prefix of) our prompt.
	p := strings.TrimSpace(prompt)
	for i := len(msgs) - 1; i >= 0; i-- {
		t := strings.ToLower(msgs[i].Type)
		if strings.Contains(t, "user") || strings.EqualFold(msgs[i].Role, "user") {
			continue
		}
		mt := strings.TrimSpace(msgs[i].Text)
		if mt == "" || mt == p || (len(mt) > 200 && strings.HasPrefix(p, mt[:200])) {
			continue
		}
		return &msgs[i]
	}
	return nil
}

// Enterprise policy notices are not answers; drop them.
var policyNotice = regexp.MustCompile(`(?im)^.*(?:wasn'?t|was not|isn'?t|could not be) (?:included|shown|returned)[^.\n]*(?:organization'?s?|admin|compliance|sensitivity|access) (?:polic|restriction|label)[^\n]*\n?`)

func stripPolicyNotice(s string) string {
	return strings.TrimSpace(policyNotice.ReplaceAllString(s, ""))
}

// Probe inspects Graph's OData $metadata for the Copilot entities and logs any
// property that looks like a model/reasoning selector. Microsoft exposes no
// model choice today; this exists so you notice the day they do — then set
// GRAPH_EXTRA_BODY='{"model":"..."}' and you're done.
func (g *GraphClient) Probe(ctx context.Context) {
	b, status, err := g.do(ctx, http.MethodGet, "/$metadata", nil)
	if err != nil || status != 200 {
		log.Printf("probe: $metadata unavailable (%v, HTTP %d) — skipping capability discovery", err, status)
		return
	}
	// Pull the EntityType/ComplexType blocks that mention copilot.
	blocks := regexp.MustCompile(`(?is)<(?:EntityType|ComplexType|Action)\s+Name="([^"]*copilot[^"]*)"[^>]*>(.*?)</(?:EntityType|ComplexType|Action)>`).FindAllStringSubmatch(string(b), -1)
	if len(blocks) == 0 {
		log.Printf("probe: no copilot types in $metadata (API may be gated or renamed)")
		return
	}
	prop := regexp.MustCompile(`(?i)(?:Property|Parameter)\s+Name="([^"]+)"`)
	interesting := regexp.MustCompile(`(?i)model|reason|deep|engine|tier|capab`)
	var hits []string
	for _, blk := range blocks {
		for _, p := range prop.FindAllStringSubmatch(blk[2], -1) {
			if interesting.MatchString(p[1]) {
				hits = append(hits, blk[1]+"."+p[1])
			}
		}
	}
	if len(hits) == 0 {
		log.Printf("probe: %d copilot types found, no model/reasoning selector exposed — Microsoft picks the model server-side", len(blocks))
		return
	}
	log.Printf("probe: POSSIBLE MODEL KNOBS in Graph schema: %s  → try GRAPH_EXTRA_BODY", strings.Join(hits, ", "))
}

// Graph's 403 for the Copilot API names the scopes it wants; surface them.
var requiredScopesRe = regexp.MustCompile(`Required scopes\s*=\s*\[([^\]]+)\]`)

// errTooLarge tells the server to shrink the prompt and try again.
var errTooLarge = errors.New("graph chat: message too large")

var tooLargeRe = regexp.MustCompile(`(?i)too (?:long|large|big)|exceed(?:s|ed)? (?:the )?(?:maximum |max )?(?:allowed )?(?:length|size|character)|(?:payload|request entity|message|text|content) (?:is )?too|maximum (?:length|size|characters?)|character limit|length limit|size limit`)

func isTooLarge(status int, body []byte) bool {
	return status == 413 || (status == 400 && tooLargeRe.Match(body))
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
