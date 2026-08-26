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
	"strconv"
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

	// --- Assemble instructions. ---
	// Field-tested: Copilot treats Graph contexts[] as reference documents and
	// does not reliably read (or obey) them. So by default every instruction —
	// persona, thinking format, the caller's system prompt, repo map, tool
	// protocol + catalog — goes INTO THE MESSAGE TEXT, the one channel it
	// always reads. INSTRUCTIONS_IN=contexts restores the old behaviour.
	system, turns := splitSystem(req.Messages)
	utility := isUtilityRequest(system, turns, len(req.Tools))
	var instr []graphContext
	if !utility {
		if pc := personaContext(s.cfg); pc != nil {
			instr = append(instr, *pc)
		}
		if s.cfg.Thinking {
			instr = append(instr, graphContext{Description: "Reply format. Always in force.", Text: thinkingInstruction})
		}
	}
	if system != "" {
		instr = append(instr, graphContext{Description: "System instructions from the calling application. Follow them.", Text: system})
	}
	root := detectRoot(s.cfg, system)
	if s.cfg.RepoMap && !utility {
		if rc := s.repo.Context(root); rc != nil {
			instr = append(instr, *rc)
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
	toolInstr := func(fresh bool) graphContext {
		text := toolProtocol + renderToolCatalog(req.Tools)
		if !fresh {
			text = renderToolReminder(req.Tools, root, recentUserText(turns, 3))
		}
		return graphContext{Description: "Tool-calling protocol. You MUST follow it exactly.", Text: text}
	}
	if len(req.Tools) > 0 {
		instr = append(instr, toolInstr(!reuse))
	}

	// Route instructions: into the message (default) or Graph contexts.
	var contexts []graphContext
	if !s.cfg.InstrInMessage {
		contexts = instr
	}
	render := func(budget int) string {
		var body string
		if reuse {
			_, delta := splitSystem(req.Messages[consumed:])
			body = renderTurns(turns, delta, true, budget)
		} else {
			body = renderTurns(turns, turns, false, budget)
		}
		if !s.cfg.InstrInMessage {
			return body
		}
		if reuse {
			// Conversation already holds the full instructions; short reminder only.
			var rem []string
			for _, c := range instr {
				if strings.HasPrefix(c.Description, "Tool-calling") {
					rem = append(rem, c.Text)
				}
			}
			if len(rem) == 0 {
				return body
			}
			return strings.Join(rem, "\n\n") + "\n\n" + body
		}
		return renderInstructions(instr) + "\n\n" + body
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
			for i := range instr {
				if strings.HasPrefix(instr[i].Description, "Tool-calling protocol") && len(req.Tools) > 0 {
					instr[i] = toolInstr(true)
				}
			}
			if !s.cfg.InstrInMessage {
				contexts = instr
			}
			continue
		}
		break
	}
	if errors.Is(err, errNoAnswer) && len(req.Tools) > 0 && s.cfg.Enforce {
		// Copilot gave nothing (policy notice / grounding failure). Nudge on
		// the same conversation instead of failing the turn.
		log.Printf("chat: Copilot returned no answer — nudging once")
		s.stats.Nudges.Add(1)
		text, sources, err = s.graph.Send(ctx, convID, enforceNudgeFor(root), contexts)
	}
	if err != nil {
		s.fail(w, err)
		return
	}

	// --- Translate reply: prose and/or tool calls; repair, enforce, re-prompt. ---
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	toolsOffered := len(req.Tools) > 0
	if !s.cfg.Sources {
		sources = ""
		text = stripCitations(text)
	}
	var reasoning string
	if s.cfg.Thinking {
		reasoning, text = splitThinking(text)
	}
	prose, calls, problems, repaired := extractToolCallsChecked(text, known, schemas)
	if len(calls) == 0 && toolsOffered {
		// Primary protocol form: ```<tool>\nbody``` blocks.
		if p2, c2 := extractTaggedCalls(text, known, primaryArgs(req.Tools)); len(c2) > 0 {
			prose, calls, problems = p2, c2, nil
		}
	}
	s.stats.Repairs.Add(int64(repaired))
	nudged := false
	synthesized := false
	// Repeat guard: the model re-proposed exactly what was just executed (its
	// output is in the history). Don't run it again — make it move on.
	if len(calls) > 0 && repeatsLastCalls(calls, req.Messages) && s.cfg.Enforce {
		log.Printf("chat: model repeated the previous call(s) %s — nudging to continue", callNames(calls))
		s.stats.Nudges.Add(1)
		if t2, s2, err2 := s.graph.Send(ctx, convID, repeatNudge, contexts); err2 == nil {
			if s.cfg.Thinking {
				_, t2 = splitThinking(t2)
			}
			p2, c2, _, _ := extractToolCallsChecked(t2, known, schemas)
			if len(c2) == 0 {
				p2, c2 = extractTaggedCalls(t2, known, primaryArgs(req.Tools))
			}
			if !repeatsLastCalls(c2, req.Messages) {
				text, sources, prose, calls, nudged = t2, s2, p2, c2, true
			} else {
				// Still looping: answer with the prose we have and stop the loop.
				calls = nil
				prose = strings.TrimSpace(p2)
				if prose == "" {
					prose = "The command was already executed; its output is shown above."
				}
				text = prose
			}
		}
	}
	if len(calls) == 0 && len(problems) == 0 && toolsOffered {
		if sh := shellToolName(known); sh != "" {
			if cmd := extractShellCommand(text); cmd != "" {
				// It told the developer to run a command: run it for them.
				calls = []parsedCall{{Name: sh, Args: json.RawMessage(`{"command":` + strconv.Quote(cmd) + `}`)}}
				prose = ""
				synthesized = true
				s.stats.Synth.Add(1)
				log.Printf("chat: synthesized %s call from prose: %q", sh, cmd)
			}
		}
	}
	if s.cfg.Enforce && toolsOffered {
		var nudge string
		switch {
		case len(calls) == 0 && len(problems) > 0:
			nudge = repairNudge(problems)
			s.stats.RepairFailures.Add(1)
			log.Printf("chat: invalid tool args (%s) — re-prompting", strings.Join(problems, "; "))
		case needsNudge(text, true, len(calls)):
			nudge = enforceNudgeFor(root)
			log.Printf("chat: reply refused/narrated instead of calling tools — nudging once")
		case !reuse && firstTurnNeedsNudge(lastUserMessage(turns), true, len(calls)):
			nudge = enforceNudgeFor(root)
			log.Printf("chat: first turn answered a task without any tool call — nudging once")
		case len(mentionedButUncalled(recentUserText(turns, 3), known, calls)) > 0:
			m := mentionedButUncalled(recentUserText(turns, 3), known, calls)
			nudge = mentionNudge(m)
			log.Printf("chat: user named tool(s) %v, reply didn't call them — nudging once", m)
		}
		if nudge != "" {
			s.stats.Nudges.Add(1)
			if t2, s2, err2 := s.graph.Send(ctx, convID, nudge, contexts); err2 == nil {
				r2think := ""
				if !s.cfg.Sources {
					s2 = ""
					t2 = stripCitations(t2)
				}
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
	if utility {
		text, sources, reasoning = cleanShortAnswer(text), "", ""
	}
	s.debugDump(id, convID, prompt, instr, text, len(calls), nudged)
	s.stats.setLast(map[string]any{
		"time": time.Now().Format(time.RFC3339), "tools_offered": len(req.Tools), "tool_calls": len(calls),
		"nudged": nudged, "synthesized_call": synthesized, "utility": utility, "reuse": reuse, "prompt_bytes": len(prompt),
		"instructions_in": map[bool]string{true: "message", false: "contexts"}[s.cfg.InstrInMessage],
		"reply_head":      redact(truncate([]byte(text), 240)),
	})
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

// renderInstructions lays the instruction blocks out as a single preamble.
func renderInstructions(instr []graphContext) string {
	var sb strings.Builder
	sb.WriteString("=== INSTRUCTIONS (read fully; they govern this entire conversation) ===\n\n")
	for _, c := range instr {
		sb.WriteString("## " + c.Description + "\n" + c.Text + "\n\n")
	}
	sb.WriteString("=== END INSTRUCTIONS ===\n\n=== CONVERSATION ===\n")
	return sb.String()
}

// mentionedButUncalled: tools the user named in their last message that the
// reply did not call. Only counts the LAST user message (a fresh ask), and
// only when the last message is from the user (not mid tool-loop).
func mentionedButUncalled(userMsg string, known map[string]bool, calls []parsedCall) []string {
	want := mentionedTools(userMsg, known)
	if len(want) == 0 {
		return nil
	}
	called := map[string]bool{}
	for _, c := range calls {
		called[c.Name] = true
	}
	var out []string
	for _, w := range want {
		if !called[w] {
			out = append(out, w)
		}
	}
	return out
}

// recentUserText joins the last n user messages — a follow-up like "list"
// inherits the tools named two turns earlier ("use localmemory mcp").
func recentUserText(msgs []oaiMessage, n int) string {
	var parts []string
	for i := len(msgs) - 1; i >= 0 && len(parts) < n; i-- {
		if msgs[i].Role == "user" {
			parts = append(parts, string(msgs[i].Content))
		}
	}
	return strings.Join(parts, "\n")
}

// repeatsLastCalls: every proposed call equals one of the most recent
// assistant tool_calls, and those calls already have results in history.
func repeatsLastCalls(calls []parsedCall, msgs []oaiMessage) bool {
	if len(calls) == 0 {
		return false
	}
	// Find the last assistant message with tool_calls, and require tool
	// results after it.
	lastIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			lastIdx = i
			break
		}
	}
	if lastIdx < 0 {
		return false
	}
	haveResult := false
	for _, m := range msgs[lastIdx+1:] {
		if m.Role == "tool" {
			haveResult = true
		}
		if m.Role == "user" {
			return false // a new user ask in between: not a repeat
		}
	}
	if !haveResult {
		return false
	}
	prev := map[string]bool{}
	for _, tc := range msgs[lastIdx].ToolCalls {
		prev[tc.Function.Name+"\x00"+canonJSON(tc.Function.Arguments)] = true
	}
	for _, c := range calls {
		if !prev[c.Name+"\x00"+canonJSON(string(c.Args))] {
			return false
		}
	}
	return true
}

func canonJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return strings.TrimSpace(s)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// instrBytes: where the prompt size goes, by instruction block.
func instrBytes(instr []graphContext) map[string]int {
	out := map[string]int{}
	for _, c := range instr {
		k := "other"
		switch {
		case strings.HasPrefix(c.Description, "Operating persona"):
			k = "persona"
		case strings.HasPrefix(c.Description, "Reply format"):
			k = "thinking"
		case strings.HasPrefix(c.Description, "System instructions"):
			k = "system_prompt"
		case strings.HasPrefix(c.Description, "Repository map"):
			k = "repo_map"
		case strings.HasPrefix(c.Description, "Tool-calling"):
			k = "tool_catalog"
		}
		out[k] += len(c.Text)
	}
	return out
}

func primaryArgs(tools []oaiTool) map[string]string {
	out := map[string]string{}
	for _, t := range tools {
		out[t.Function.Name] = primaryArg(t.Function.Parameters)
	}
	return out
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
