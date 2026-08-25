// m365-copilot-proxy
//
// OpenAI-compatible local front door for Microsoft 365 Copilot, with a
// tool-calling shim so OpenCode runs as a full coding agent on top of it.
//
//   OpenCode --OpenAI JSON (+tools)--> :8080/v1/chat/completions --Graph--> Copilot
//   Copilot  --text with ```tool_call blocks--> proxy --OpenAI tool_calls--> OpenCode
//   OpenCode executes read/grep/edit/bash LOCALLY, sends role:"tool" results, loop.
//
// Files: config.go (env), auth.go (Entra OAuth2), openai.go (wire types),
// tools.go (tool-call protocol + parser), convo.go (conversation reuse and
// prompt rendering), graph.go (Graph client + probe), main.go (HTTP server).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	cfg   Config
	graph *GraphClient
	convs *ConvCache
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
	if system != "" {
		contexts = append(contexts, graphContext{
			Description: "System instructions from the calling application. Follow them.",
			Text:        system,
		})
	}
	known := map[string]bool{}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			known[t.Function.Name] = true
		}
		contexts = append(contexts, graphContext{
			Description: "Tool-calling protocol. You MUST follow it exactly.",
			Text:        toolProtocol + renderToolCatalog(req.Tools),
		})
	}

	// --- Conversation reuse: send only what this Graph conversation hasn't seen. ---
	convID, consumed := s.convs.Lookup(req.Messages)
	var prompt string
	if convID != "" && consumed <= len(req.Messages) {
		_, deltaTurns := splitSystem(req.Messages[consumed:])
		prompt = renderTurns(turns, deltaTurns, true)
		log.Printf("chat: reusing conversation %s (+%d msgs)", shortID(convID), len(req.Messages)-consumed)
	} else {
		id, err := s.graph.NewConversation(ctx)
		if err != nil {
			log.Printf("chat: %v", err)
			writeOpenAIError(w, http.StatusBadGateway, err.Error())
			return
		}
		convID = id
		prompt = renderTurns(turns, turns, false)
		log.Printf("chat: new conversation %s (%d msgs, %d tools)", shortID(convID), len(req.Messages), len(req.Tools))
	}

	text, sources, err := s.graph.Send(ctx, convID, prompt, contexts)
	if err != nil {
		log.Printf("chat: %v", err)
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}

	// --- Translate reply: prose and/or tool calls. ---
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	prose, calls := extractToolCalls(text, known)
	reply := oaiMessage{Role: "assistant"}
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
	srv := &Server{cfg: cfg, graph: graph, convs: NewConvCache(cfg.ConvTTL)}

	// Warm the token now so the device-code prompt appears at startup, not
	// mid-request; then probe Graph for anything model-selector-shaped.
	if _, err := auth.Token(context.Background()); err != nil {
		log.Fatalf("initial auth failed: %v", err)
	}
	go graph.Probe(context.Background())

	log.Printf("m365-copilot-proxy on http://%s  model=%s auth=%s tools=shim conv-reuse=on", cfg.Listen, cfg.ModelName, cfg.AuthMode)
	log.Fatal(http.ListenAndServe(cfg.Listen, srv.routes()))
}
