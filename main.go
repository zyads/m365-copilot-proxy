// m365-copilot-proxy
//
// OpenAI-compatible local front door for Microsoft 365 Copilot, with a
// tool-calling shim so OpenCode runs as a full coding agent on top of it.
//
//	OpenCode --OpenAI JSON (+tools)--> :8080/v1/chat/completions --Graph--> Copilot
//	Copilot  --text with ```tool_call blocks--> proxy --OpenAI tool_calls--> OpenCode
//	OpenCode executes read/grep/edit/bash LOCALLY, sends role:"tool" results, loop.
//
// Files: config.go (env), auth.go (Entra OAuth2), openai.go (wire types),
// tools.go (tool-call protocol + parser), convo.go (conversation reuse and
// prompt rendering), graph.go (Graph client + probe), main.go (HTTP server).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	cfg   Config
	graph *GraphClient
	convs *ConvCache
	repo  *RepoMapper
	stats Stats
	auth  *Authenticator
	upd   *Updater
	locks sync.Map // convID -> *sync.Mutex; serialises writes to one Graph conversation
}

func (s *Server) lockConv(id string) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/stats", s.stats.handler)
	mux.HandleFunc("/auth", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.auth.Status()) })
	mux.HandleFunc("/auth/login", s.auth.loginHandler)
	mux.HandleFunc("/auth/callback", s.auth.callbackHandler)
	mux.HandleFunc("/auth/seed", s.auth.seedHandler)
	mux.HandleFunc("/update", s.upd.handler)
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		local, _, _, _ := s.upd.Check()
		writeJSON(w, 200, map[string]any{"commit": short(local), "repo": s.upd.repo})
	})
	return s.upd.track(logMiddleware(mux))
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

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
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "messages[] is empty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	// --- Build contexts: caller's system prompt + tool protocol (if tools). ---
	system, turns := splitSystem(req.Messages)
	var contexts []graphContext
	if pc := personaContext(s.cfg); pc != nil {
		contexts = append(contexts, *pc) // first, so it frames everything after
	}
	if s.cfg.Thinking {
		contexts = append(contexts, graphContext{Description: "Reply format. Always in force.", Text: thinkingInstruction})
	}
	if system != "" {
		contexts = append(contexts, graphContext{
			Description: "System instructions from the calling application. Follow them.",
			Text:        system,
		})
	}
	if s.cfg.RepoMap {
		if rc := s.repo.Context(detectRoot(s.cfg, system)); rc != nil {
			contexts = append(contexts, *rc)
		}
	}
	known := map[string]bool{}
	for _, t := range req.Tools {
		known[t.Function.Name] = true
	}

	// --- Conversation reuse: send only what this Graph conversation hasn't seen. ---
	schemas := parseSchemas(req.Tools)
	convID, consumed := s.convs.Lookup(req.Messages)
	reuse := convID != "" && consumed <= len(req.Messages)
	if !reuse {
		id, err := s.graph.NewConversation(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		convID = id
		s.stats.NewConvs.Add(1)
		log.Printf("chat: new conversation %s (%d msgs, %d tools)", shortID(convID), len(req.Messages), len(req.Tools))
	} else {
		s.stats.ReusedConvs.Add(1)
		log.Printf("chat: reusing conversation %s (+%d msgs)", shortID(convID), len(req.Messages)-consumed)
	}
	unlock := s.lockConv(convID)
	defer func() { unlock() }() // closure: the replay path swaps the lock

	// Tool protocol: full catalog on a fresh conversation, names-only reminder
	// on reuse (Copilot retains the definitions in conversation state).
	if len(req.Tools) > 0 {
		text := toolProtocol + renderToolCatalog(req.Tools)
		if reuse {
			text = renderToolReminder(req.Tools)
		}
		contexts = append(contexts, graphContext{Description: "Tool-calling protocol. You MUST follow it exactly.", Text: text})
	}

	render := func(budget int) string {
		if reuse {
			_, delta := splitSystem(req.Messages[consumed:])
			return renderTurns(turns, delta, true, budget)
		}
		return renderTurns(turns, turns, false, budget)
	}

	// --- Send, shrinking the tool-result budget if Graph says "too large". ---
	// If a REUSED conversation errors for any other reason (undocumented turn
	// cap, expired server-side, etc.), drop it, open a fresh one, and replay
	// the folded history so a long agent task doesn't die mid-flight.
	started := time.Now()
	budget := s.cfg.ToolResultMax
	var prompt, text, sources string
	var err error
	for attempt := 0; ; attempt++ {
		prompt = render(budget)
		text, sources, err = s.graph.Send(ctx, convID, prompt, contexts)
		if err == nil || attempt >= 3 {
			break
		}
		if errors.Is(err, errTooLarge) {
			budget /= 2
			s.stats.Shrinks.Add(1)
			log.Printf("chat: message too large — retrying with tool-result budget %d", budget)
			continue
		}
		if reuse {
			log.Printf("chat: reused conversation %s failed (%v) — replaying into a fresh one", shortID(convID), err)
			unlock()
			id, nerr := s.graph.NewConversation(ctx)
			if nerr != nil {
				err = nerr
				break
			}
			convID, reuse = id, false
			unlock = s.lockConv(convID)
			s.stats.NewConvs.Add(1)
			s.stats.Replays.Add(1)
			// Fresh conversation needs the full tool catalog, not the reminder.
			for i := range contexts {
				if strings.HasPrefix(contexts[i].Description, "Tool-calling protocol") && len(req.Tools) > 0 {
					contexts[i].Text = toolProtocol + renderToolCatalog(req.Tools)
				}
			}
			continue
		}
		break
	}
	if err != nil {
		s.fail(w, err)
		return
	}

	// --- Translate reply: prose and/or tool calls; repair, enforce, re-prompt. ---
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	toolsOffered := len(req.Tools) > 0
	var reasoning string
	if s.cfg.Thinking {
		reasoning, text = splitThinking(text)
	}
	prose, calls, problems, repaired := extractToolCallsChecked(text, known, schemas)
	s.stats.Repairs.Add(int64(repaired))
	nudged := false
	if s.cfg.Enforce && toolsOffered {
		var nudge string
		switch {
		case len(calls) == 0 && len(problems) > 0:
			nudge = repairNudge(problems)
			s.stats.RepairFailures.Add(1)
			log.Printf("chat: invalid tool args (%s) — re-prompting", strings.Join(problems, "; "))
		case needsNudge(text, true, len(calls)):
			nudge = enforceNudge
			log.Printf("chat: reply refused/narrated instead of calling tools — nudging once")
		case !reuse && firstTurnNeedsNudge(lastUserMessage(turns), true, len(calls)):
			nudge = enforceNudge
			log.Printf("chat: first turn answered a task without any tool call — nudging once")
		}
		if nudge != "" {
			s.stats.Nudges.Add(1)
			if t2, s2, err2 := s.graph.Send(ctx, convID, nudge, contexts); err2 == nil {
				r2think := ""
				if s.cfg.Thinking {
					r2think, t2 = splitThinking(t2)
				}
				if p2, c2, _, r2 := extractToolCallsChecked(t2, known, schemas); len(c2) > 0 {
					text, sources, prose, calls, nudged = t2, s2, p2, c2, true
					if r2think != "" {
						reasoning = strings.TrimSpace(reasoning + "\n\n" + r2think)
					}
					s.stats.Repairs.Add(int64(r2))
				}
			} else {
				log.Printf("chat: nudge failed: %v", err2)
			}
		}
	}
	s.debugDump(id, convID, prompt, contexts, text, len(calls), nudged)
	s.stats.Turns.Add(1)
	s.stats.ToolCalls.Add(int64(len(calls)))
	s.stats.observe(time.Since(started))

	reply := oaiMessage{Role: "assistant", Reasoning: reasoning, Reasoning2: reasoning}
	finish := "stop"
	if len(calls) > 0 {
		reply.ToolCalls = toOpenAIToolCalls(calls, id[len(id)-8:])
		reply.Content = oaiContent(prose)
		finish = "tool_calls"
		log.Printf("chat: %d tool call(s): %s", len(calls), callNames(calls))
	} else {
		reply.Content = oaiContent(text + sources) // sources only on final answers
	}
	s.convs.Remember(convID, req.Messages, reply)

	model := req.Model
	if model == "" {
		model = s.cfg.ModelName
	}
	usage := &oaiUsage{PromptTokens: approxTokens(prompt), CompletionTokens: approxTokens(text)}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	if req.Stream {
		s.writeStream(w, id, model, reply, finish, usage)
		return
	}
	writeJSON(w, http.StatusOK, oaiResponse{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []oaiChoice{{Index: 0, Message: &reply, FinishReason: &finish}},
		Usage:   usage,
	})
}

// writeStream emits the reply as OpenAI SSE: role delta, content deltas,
// tool_call deltas, finish, [DONE].
func (s *Server) writeStream(w http.ResponseWriter, id, model string, reply oaiMessage, finish string, usage *oaiUsage) {
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
	chunk := func(delta *oaiMessage, fin *string) oaiResponse {
		return oaiResponse{
			ID: id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: model,
			Choices: []oaiChoice{{Index: 0, Delta: delta, FinishReason: fin}},
		}
	}
	emit(chunk(&oaiMessage{Role: "assistant", Content: ""}, nil))
	for _, piece := range splitChunks(reply.Reasoning, 32) {
		emit(chunk(&oaiMessage{Reasoning: piece, Reasoning2: piece}, nil))
	}
	for _, piece := range splitChunks(string(reply.Content), 24) {
		emit(chunk(&oaiMessage{Content: oaiContent(piece)}, nil))
	}
	for _, tc := range reply.ToolCalls {
		emit(chunk(&oaiMessage{ToolCalls: []oaiToolCall{tc}}, nil))
	}
	final := chunk(&oaiMessage{}, &finish)
	final.Usage = usage
	emit(final)
	bw.WriteString("data: [DONE]\n\n")
	bw.Flush()
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.stats.Errors.Add(1)
	log.Printf("chat: %v", err)
	writeOpenAIError(w, http.StatusBadGateway, err.Error())
}

// debugDump writes one JSON file per turn to DEBUG_DIR — the exact prompt
// and contexts Copilot saw and the raw text it returned. Use it to tune the
// protocol against the real model.
func (s *Server) debugDump(id, convID, prompt string, contexts []graphContext, reply string, calls int, nudged bool) {
	if s.cfg.DebugDir == "" {
		return
	}
	_ = os.MkdirAll(s.cfg.DebugDir, 0o700)
	b, _ := json.MarshalIndent(map[string]any{
		"id": id, "conversation": convID, "time": time.Now().Format(time.RFC3339),
		"contexts": contexts, "prompt": prompt, "reply": reply, "tool_calls": calls, "nudged": nudged,
		"redacted": !s.cfg.DebugRaw,
	}, "", "  ")
	if !s.cfg.DebugRaw {
		b = []byte(redact(string(b)))
	}
	_ = os.WriteFile(filepath.Join(s.cfg.DebugDir, id+".json"), b, 0o600)
}

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

func callNames(calls []parsedCall) string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Name
	}
	return strings.Join(names, ",")
}

func lastUserMessage(msgs []oaiMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return string(msgs[i].Content)
		}
	}
	return ""
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "proxy_error", "code": status},
	})
}

func main() {
	cfg := loadConfig()
	if cfg.ClientID == "" {
		log.Fatal("M365_CLIENT_ID is required (app registration client id)")
	}
	auth := NewAuthenticator(cfg)
	graph := &GraphClient{cfg: cfg, auth: auth, http: &http.Client{Timeout: cfg.RequestTimeout}}
	upd := NewUpdater(cfg)
	srv := &Server{cfg: cfg, graph: graph, convs: NewConvCache(cfg.ConvTTL), repo: NewRepoMapper(), auth: auth, upd: upd}

	// Sign in in the background: on a terminal the code prints here; as a
	// service the launcher reads it from /auth. Then probe Graph.
	auth.EnsureAsync()
	go func() {
		for i := 0; i < 600; i++ { // wait up to 10 min for sign-in before probing
			if st := auth.Status(); st["authenticated"] == true {
				graph.Probe(context.Background())
				return
			}
			time.Sleep(time.Second)
		}
	}()
	go upd.Loop(context.Background())
	if upd.repo != "" {
		log.Printf("update: git install at %s (auto=%v every %s)", upd.repo, cfg.AutoUpdate, cfg.UpdateInterval)
	}

	log.Printf("m365-copilot-proxy on http://%s  model=%s auth=%s tools=shim conv-reuse=on repomap=%v enforce=%v", cfg.Listen, cfg.ModelName, cfg.AuthMode, cfg.RepoMap, cfg.Enforce)
	log.Fatal(http.ListenAndServe(cfg.Listen, srv.routes()))
}
