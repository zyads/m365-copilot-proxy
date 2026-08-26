package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepairJSON(t *testing.T) {
	cases := map[string]string{
		`{"path": "a",}`:                          `{"path":"a"}`,
		`{'path': 'a'}`:                           `{"path":"a"}`,
		"{\"content\": \"line1\nline2\"}":         `{"content":"line1\nline2"}`,
		"```json\n{\"path\": \"a\"}\n```":         `{"path":"a"}`,
		`here you go: {"path": "a", "n": 1} done`: `{"n":1,"path":"a"}`,
	}
	for in, want := range cases {
		got, ok := repairJSON(in)
		if !ok || string(got) != want {
			t.Errorf("repairJSON(%q) = %s ok=%v, want %s", in, got, ok, want)
		}
	}
	if _, ok := repairJSON("not json at all"); ok {
		t.Error("garbage should not repair")
	}
}

func TestExtractWithRepairAndValidation(t *testing.T) {
	known := map[string]bool{"read": true, "edit": true}
	schemas := parseSchemas([]oaiTool{mkTool("read", `{"type":"object","required":["path"],"properties":{"path":{}}}`),
		mkTool("edit", `{"type":"object","required":["path","content"],"properties":{"path":{},"content":{}}}`)})
	text := "```tool_call\n{\"name\": \"read\", \"arguments\": {\"path\": \"a.go\",}}\n```\n" + // trailing comma → repaired
		"```tool_call\n{\"name\": \"edit\", \"arguments\": {\"path\": \"b.go\"}}\n```\n" + // missing content → problem
		"```tool_call\n{\"name\": \"read\", \"arguments\": {\"filepath\": \"c.go\"}}\n```" // wrong key → problem
	_, calls, problems, repaired := extractToolCallsChecked(text, known, schemas)
	if len(calls) != 1 || calls[0].Name != "read" || string(calls[0].Args) != `{"path":"a.go"}` {
		t.Fatalf("calls: %+v", calls)
	}
	if repaired != 1 {
		t.Errorf("repaired=%d", repaired)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`edit: missing required argument "content"`, `read: missing required argument "path"`, `read: unknown argument "filepath"`, `edit: exact schema:`, `read: exact schema:`} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems missing %q: %v", want, problems)
		}
	}
}

func mkTool(name, params string) oaiTool {
	var t oaiTool
	t.Type = "function"
	t.Function.Name = name
	t.Function.Parameters = json.RawMessage(params)
	return t
}

func TestCompactAndFold(t *testing.T) {
	big := strings.Repeat("line of output\n", 1000) // 15k
	c := compactResult(big, 3000)
	if len(c) > 3200 || !strings.HasPrefix(c, "line of output") || !strings.HasSuffix(c, "line of output\n") || !strings.Contains(c, "bytes omitted") {
		t.Errorf("compact: len=%d %q…%q", len(c), c[:30], c[len(c)-30:])
	}
	if compactResult("small", 100) != "small" {
		t.Error("small must be untouched")
	}
	var msgs []oaiMessage
	for i := 0; i < 10; i++ {
		msgs = append(msgs, oaiMessage{Role: "tool", ToolCallID: "c", Content: oaiContent(strings.Repeat("x", 500) + "\nend")})
	}
	f := foldHistory(msgs, 3)
	if !strings.HasPrefix(string(f[0].Content), "[folded: 504 bytes, 2 lines]") || len(f[9].Content) != 504 {
		t.Errorf("fold: first=%q last-len=%d", f[0].Content[:40], len(f[9].Content))
	}
	if len(msgs[0].Content) != 504 {
		t.Error("foldHistory must not mutate input")
	}
}

// Graph rejects the first send as too large → proxy halves the budget and
// retries on the same conversation → succeeds.
func TestTooLargeShrinkRetry(t *testing.T) {
	sends := 0
	var sizes []int
	g := newFakeGraph(t, func(p string) string { return "final" })
	// Wrap: reject the first chat call with a 400 "too long".
	orig := g.Server.Config.Handler
	g.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat") {
			sends++
			var body struct {
				Message struct{ Text string } `json:"message"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			sizes = append(sizes, len(body.Message.Text))
			if sends == 1 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"code":"BadRequest","message":"Message text is too long"}}`))
				return
			}
			w.Write([]byte(`{"id":"conv-1","messages":[{"id":"m","text":"final"}]}`))
			return
		}
		orig.ServeHTTP(w, r)
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	huge := strings.Repeat("abc ", 20000)
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[{"role":"user","content":"go"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"`+huge+`"}]}`)
	if sends != 2 || sizes[1] >= sizes[0] || string(out.Choices[0].Message.Content) != "final" {
		t.Fatalf("sends=%d sizes=%v out=%+v", sends, sizes, out)
	}
}

// Bad arguments → repair nudge with the precise problem → corrected call.
func TestArgRepairNudge(t *testing.T) {
	var prompts []string
	g := newFakeGraph(t, func(p string) string {
		prompts = append(prompts, p)
		if strings.Contains(p, "Your tool_call arguments were invalid") {
			return "```tool_call\n{\"name\":\"read\",\"arguments\":{\"path\":\"ok.go\"}}\n```"
		}
		return "```tool_call\n{\"name\":\"read\",\"arguments\":{\"file\":\"ok.go\"}}\n```"
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	strictTools := `"tools":[{"type":"function","function":{"name":"read","description":"Read","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}}]`
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+strictTools+`,"messages":[{"role":"user","content":"read it"}]}`)
	m := out.Choices[0].Message
	if len(prompts) != 2 || !strings.Contains(prompts[1], `unknown argument "file"`) || !strings.Contains(prompts[1], `missing required argument "path"`) {
		t.Fatalf("nudge prompt wrong: %v", prompts)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Arguments != `{"path":"ok.go"}` {
		t.Fatalf("corrected call missing: %+v", m)
	}
	// /stats reflects it.
	resp, _ := http.Get(p.URL + "/stats")
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["nudges"].(float64) != 1 || st["arg_repair_failures"].(float64) != 1 || st["turns"].(float64) != 1 {
		t.Errorf("stats: %v", st)
	}
}

func TestThinkingSplit(t *testing.T) {
	r, rest := splitThinking("<thinking>need to read the file</thinking>\n```tool_call\n{\"name\":\"read\",\"arguments\":{\"path\":\"a\"}}\n```")
	if r != "need to read the file" || !strings.HasPrefix(rest, "```tool_call") {
		t.Errorf("closed: %q | %q", r, rest)
	}
	r, rest = splitThinking("<thinking>ran out of")
	if r != "ran out of" || rest != "" {
		t.Errorf("unclosed: %q | %q", r, rest)
	}
	r, rest = splitThinking("plain answer")
	if r != "" || rest != "plain answer" {
		t.Errorf("none: %q | %q", r, rest)
	}
}

func TestThinkingSurfacedAsReasoning(t *testing.T) {
	g := newFakeGraph(t, func(string) string {
		return "<thinking>list first, then decide</thinking>\n```tool_call\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n```"
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[{"role":"user","content":"go"}]}`)
	m := out.Choices[0].Message
	if m.Reasoning != "list first, then decide" || len(m.ToolCalls) != 1 || strings.Contains(string(m.Content), "thinking") {
		t.Fatalf("reasoning not split: %+v", m)
	}
	if !strings.Contains(g.prompts[0], "<thinking>") {
		t.Error("thinking instruction not sent")
	}
	// Streaming carries reasoning_content deltas before content.
	resp, _ := post(t, p.URL, `{"model":"m365-copilot","stream":true,`+tools+`,"messages":[{"role":"user","content":"go again"}]}`)
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	ri, ti := strings.Index(body, `"reasoning_content":"list first`), strings.Index(body, `"tool_calls"`)
	if ri < 0 || ti < 0 || ri > ti {
		t.Errorf("stream order wrong: reasoning@%d tool_calls@%d", ri, ti)
	}
}

// Reused conversation hits an (undocumented) turn cap → proxy opens a new
// conversation, replays history with the full tool catalog, and succeeds.
func TestReplayOnDeadConversation(t *testing.T) {
	chats := 0
	var convs []string
	var lastPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/copilot/conversations":
			id := "conv-" + string(rune('A'+len(convs)))
			convs = append(convs, id)
			w.Write([]byte(`{"id":"` + id + `"}`))
		case strings.HasSuffix(r.URL.Path, "/chat"):
			chats++
			var body struct {
				Message struct{ Text string } `json:"message"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			lastPrompt = body.Message.Text
			if strings.Contains(r.URL.Path, "conv-A") && chats == 2 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"code":"BadRequest","message":"Conversation has reached the maximum number of turns"}}`))
				return
			}
			w.Write([]byte(`{"id":"x","messages":[{"id":"m","text":"ok"}]}`))
		}
	}))
	defer srv.Close()
	p := newProxy(t, srv.URL)
	defer p.Close()
	// Turn 1 → conv-A.
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[{"role":"user","content":"t1"}]}`)
	if len(convs) != 1 || string(out.Choices[0].Message.Content) != "ok" {
		t.Fatalf("turn1: convs=%v", convs)
	}
	// Turn 2 reuses conv-A, which now 400s ("maximum turns") → replay into conv-B.
	_, out = post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[{"role":"user","content":"t1"},{"role":"assistant","content":"ok"},{"role":"user","content":"t2"}]}`)
	if len(convs) != 2 || string(out.Choices[0].Message.Content) != "ok" {
		t.Fatalf("replay failed: convs=%v out=%+v", convs, out)
	}
	if !strings.Contains(lastPrompt, "### read") || !strings.Contains(lastPrompt, defaultPersona) {
		t.Errorf("replay should carry full instructions + catalog, got: %.120q", lastPrompt)
	}
	resp, _ := http.Get(p.URL + "/stats")
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["conversation_replays"].(float64) != 1 {
		t.Errorf("stats: %v", st)
	}
}

func TestUtilityTitleRequest(t *testing.T) {
	g := newFakeGraph(t, func(p string) string {
		if strings.Contains(p, defaultPersona) {
			return "SHOULD NOT HAVE PERSONA"
		}
		return "<thinking>short</thinking>\n**Conversation title:** \"Friendly greetng.\""
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	_, out := post(t, p.URL, `{"model":"m365-copilot","messages":[{"role":"system","content":"Generate a short title for this conversation. Respond with the title only."},{"role":"user","content":"hey there"}]}`)
	if got := string(out.Choices[0].Message.Content); got != "Friendly greetng" {
		t.Fatalf("title not cleaned: %q", got)
	}
	if out.Choices[0].Message.Reasoning != "" {
		t.Error("utility reply must not carry reasoning")
	}
	for in, want := range map[string]string{
		"Title: Fix login bug":             "Fix login bug",
		"\"Refactor auth\"":                "Refactor auth",
		"**Suggested title** — Add tests.": "Add tests",
		"Plain title\nwith a second line":  "Plain title",
	} {
		if got := cleanShortAnswer(in); got != want {
			t.Errorf("cleanShortAnswer(%q)=%q want %q", in, got, want)
		}
	}
}

func TestStripCitations(t *testing.T) {
	in := "The migration is half done [1][2]. Next step is BUILD files [3].\n\n**Sources:**\n1. Teams message from Someone\n2. https://contoso.sharepoint.com/x\n"
	want := "The migration is half done. Next step is BUILD files."
	if got := stripCitations(in); got != want {
		t.Errorf("got %q", got)
	}
	keep := "```tool_call\n{\"name\":\"read\",\"arguments\":{\"path\":\"a[1].go\"}}\n```"
	if got := stripCitations(keep); got != keep {
		t.Errorf("tool block mangled: %q", got)
	}
	if got := stripCitations("arr[0] and list[12] in code"); got != "arr[0] and list[12] in code" {
		t.Errorf("code indices mangled: %q", got)
	}
}

func TestEnterpriseGroundingTells(t *testing.T) {
	for _, txt := range []string{
		"I can't determine the current failure set from records alone.",
		"Based on the Teams messages, the rollout stalled in March.",
		"Searching enterprise sources, I found a related SharePoint doc.",
		"From our conversation I don't see any migration details.",
	} {
		if !needsNudge(txt, true, 0) {
			t.Errorf("should nudge: %q", txt)
		}
	}
}

func TestToolNameResolutionAndMention(t *testing.T) {
	known := map[string]bool{"local-memory_search": true, "local-memory_store": true, "read": true, "todoread": true}
	for in, want := range map[string]string{
		"local-memory.search": "local-memory_search",
		"Local_Memory_Search": "local-memory_search",
		"search":              "local-memory_search",
		"store":               "local-memory_store",
		"memory":              "", // ambiguous
		"nope":                "",
	} {
		if got := resolveToolName(in, known); got != want {
			t.Errorf("resolve(%q)=%q want %q", in, got, want)
		}
	}
	_, calls, problems, repaired := extractToolCallsChecked("```tool_call\n{\"name\":\"local-memory.search\",\"arguments\":{\"q\":\"plan\"}}\n```", known, nil)
	if len(calls) != 1 || calls[0].Name != "local-memory_search" || repaired != 1 || len(problems) != 0 {
		t.Errorf("mangled name not resolved: %+v %v", calls, problems)
	}
	got := mentionedTools("use local-memory to recall the plan, and check todoread", known)
	if strings.Join(got, ",") != "local-memory_search,local-memory_store,todoread" {
		t.Errorf("mentioned = %v", got)
	}
	if m := mentionedButUncalled("check todoread", known, []parsedCall{{Name: "todoread"}}); len(m) != 0 {
		t.Errorf("called tool flagged: %v", m)
	}
}

func TestBigCatalogScales(t *testing.T) {
	var ts []oaiTool
	for _, n := range []string{"read", "bash", "edit", "todoread"} {
		ts = append(ts, mkTool(n, `{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`))
	}
	for s := 0; s < 12; s++ {
		for i := 0; i < 20; i++ {
			tl := mkTool(fmt.Sprintf("server%d_tool%d", s, i), `{"type":"object","required":["q"],"properties":{"q":{"type":"string","description":"`+strings.Repeat("long ", 80)+`"}}}`)
			tl.Function.Description = strings.Repeat("blah ", 200)
			ts = append(ts, tl)
		}
	}
	lm := mkTool("local-memory_search", `{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`)
	lm.Function.Description = "Search local memory"
	ts = append(ts, lm, mkTool("local-memory_store", `{"type":"object"}`))
	cat := renderToolCatalog(ts)
	if len(cat) > 60_000 {
		t.Errorf("catalog too big for %d tools: %d bytes", len(ts), len(cat))
	}
	if !strings.Contains(cat, "### read") || !strings.Contains(cat, "#### local-memory (2)") || !strings.Contains(cat, "- local-memory_search: Search local memory") {
		t.Errorf("catalog shape wrong")
	}
	if strings.Contains(cat, `"q":{"type":"string","description":"long`) {
		t.Errorf("MCP schemas should be omitted in a big catalog")
	}
	rem := renderToolReminder(ts, "/w/repo", "use local-memory to recall the plan")
	if len(rem) > 6_000 || !strings.Contains(rem, "server0 (20)") || !strings.Contains(rem, "### local-memory_search") || !strings.Contains(rem, `"query"`) {
		t.Errorf("reminder wrong (%d bytes): %.400s", len(rem), rem)
	}
	// Bad args to an MCP tool → the exact schema comes back.
	sch := parseSchemas(ts)
	_, _, problems, _ := extractToolCallsChecked("```tool_call\n{\"name\":\"local-memory_search\",\"arguments\":{}}\n```", map[string]bool{"local-memory_search": true}, sch)
	if len(problems) != 2 || !strings.Contains(problems[1], `exact schema: `) || !strings.Contains(problems[1], `"required":["query"]`) {
		t.Errorf("schema on demand missing: %v", problems)
	}
}

func TestPickResponseNeverEchoesPrompt(t *testing.T) {
	prompt := "=== STANDING ORDERS === lots of text " + strings.Repeat("x", 300)
	// Typed messages: pick by type regardless of order.
	msgs := []graphMessage{
		{Type: "#microsoft.graph.copilotResponseMessage", Text: "real answer"},
		{Type: "#microsoft.graph.copilotUserMessage", Text: prompt},
	}
	if m := pickResponse(msgs, prompt); m == nil || m.Text != "real answer" {
		t.Errorf("typed pick failed: %+v", m)
	}
	// Untyped, only our prompt echoed back → nil (not the prompt).
	if m := pickResponse([]graphMessage{{Text: prompt}}, prompt); m != nil {
		t.Errorf("echoed prompt returned as answer: %q", m.Text[:40])
	}
	// Untyped user echo + answer → answer.
	if m := pickResponse([]graphMessage{{Text: prompt}, {Text: "ok"}}, prompt); m == nil || m.Text != "ok" {
		t.Errorf("untyped pick failed")
	}
	// Policy notice is not an answer.
	if got := stripPolicyNotice("⚠ Some relevant content wasn't included due to your organization's policies. Learn about access restrictions https://learn.microsoft.com/x\n"); got != "" {
		t.Errorf("policy notice survived: %q", got)
	}
	if got := stripPolicyNotice("Some content wasn't included due to your organization's policies.\nHere is the answer."); got != "Here is the answer." {
		t.Errorf("mixed: %q", got)
	}
}

// Graph echoes only our prompt (no answer) → proxy nudges on the same
// conversation instead of returning the prompt as the reply.
func TestNoAnswerNudges(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/copilot/conversations" {
			w.Write([]byte(`{"id":"c"}`))
			return
		}
		n++
		var body struct {
			Message struct{ Text string } `json:"message"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		if n == 1 {
			json.NewEncoder(w).Encode(map[string]any{"id": "c", "messages": []map[string]any{{"id": "u", "text": body.Message.Text}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "c", "messages": []map[string]any{
			{"id": "u", "text": body.Message.Text},
			{"id": "a", "text": "```tool_call\n{\"name\":\"bash\",\"arguments\":{\"command\":\"git status\"}}\n```"}}})
	}))
	defer srv.Close()
	p := newProxy(t, srv.URL)
	defer p.Close()
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[{"role":"user","content":"what happened"}]}`)
	if n != 2 || *out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("sends=%d finish=%s content=%.80q", n, *out.Choices[0].FinishReason, out.Choices[0].Message.Content)
	}
}

func TestMentionNormalisedAndRecent(t *testing.T) {
	known := map[string]bool{"local-memory_list": true, "local-memory_recall": true, "read": true}
	for _, msg := range []string{"use localmemory mcp", "use local_memory", "the LOCAL-MEMORY server", "local memory please"} {
		if got := mentionedTools(msg, known); len(got) != 2 {
			t.Errorf("%q → %v", msg, got)
		}
	}
	if got := mentionedTools("read the docs then proceed", known); len(got) != 1 || got[0] != "read" {
		t.Errorf("read: %v", got)
	}
	turns := []oaiMessage{{Role: "user", Content: "use localmemory mcp"}, {Role: "assistant", Content: "what op?"}, {Role: "user", Content: "list"}}
	if !strings.Contains(recentUserText(turns, 3), "localmemory") {
		t.Error("recent text lost earlier turn")
	}
	for _, txt := range []string{
		"I can't list local-memory entries from here because no accessible local-memory tool is available in this session.",
		"That tool isn't exposed in this session's toolset.",
		"The local-memory tools are not available in the current context.",
	} {
		if !needsNudge(txt, true, 0) {
			t.Errorf("should nudge: %q", txt)
		}
	}
}

func TestParserAcceptsWhatModelsActuallyWrite(t *testing.T) {
	known := map[string]bool{"bash": true, "read": true}
	cases := []string{
		"```json\n{\"name\": \"bash\", \"arguments\": {\"command\": \"git status\"}}\n```",
		"```\n{\"name\": \"bash\", \"arguments\": {\"command\": \"git status\"}}\n```",
		"tool_call: {\"name\": \"bash\", \"arguments\": {\"command\": \"git status\"}}",
		"Let me check.\n{\"name\": \"bash\", \"arguments\": {\"command\": \"git status\"}}\nWaiting.",
		"```tool_call\n{\"name\":\"bash\",\"arguments\":{\"command\":\"git status\"}}\n```",
	}
	for _, c := range cases {
		_, calls := extractToolCalls(c, known)
		if len(calls) != 1 || calls[0].Name != "bash" {
			t.Errorf("not parsed: %q → %+v", c, calls)
		}
	}
	// Ordinary JSON / code fences must NOT become calls.
	for _, c := range []string{
		"```json\n{\"version\": 1, \"deps\": []}\n```",
		"```go\nfunc main() {}\n```",
		"config: {\"name\": \"app\"}",
	} {
		if _, calls := extractToolCalls(c, known); len(calls) != 0 {
			t.Errorf("false positive: %q → %+v", c, calls)
		}
	}
}

func TestExtractShellCommand(t *testing.T) {
	cases := map[string]string{
		"I can't access the repository path from this environment. Run:\n\ngit status --short --branch\n\nfrom your repo root and paste the output.": "git status --short --branch",
		"Run `git log --oneline -5` and paste it.":     "git log --oneline -5",
		"```bash\n$ go test ./...\n```":                "go test ./...",
		"```sh\ncd pkg\nls -la\n```":                   "cd pkg && ls -la",
		"Here is the fix:\n```go\nfunc main() {}\n```": "",
		"Run either `git status` or `git diff`.":       "", // ambiguous → no synth
		"The migration is complete.":                   "",
	}
	for in, want := range cases {
		if got := extractShellCommand(in); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

func TestSynthesizedCallFromProse(t *testing.T) {
	g := newFakeGraph(t, func(string) string {
		return "I can't access the repository path from this environment. Run:\n\ngit status --short --branch\n\nfrom your repo root and paste the output if you want help interpreting it."
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[{"role":"user","content":"run git status w the bash tool"}]}`)
	m := out.Choices[0].Message
	if *out.Choices[0].FinishReason != "tool_calls" || len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "bash" || !strings.Contains(m.ToolCalls[0].Function.Arguments, "git status --short --branch") {
		t.Fatalf("no synthesized call: %+v", out)
	}
}

func TestTaggedFenceCalls(t *testing.T) {
	ts := []oaiTool{
		mkTool("bash", `{"type":"object","required":["command"],"properties":{"command":{},"timeout":{}}}`),
		mkTool("read", `{"type":"object","required":["filePath"],"properties":{"filePath":{}}}`),
		mkTool("grep", `{"type":"object","required":["pattern"],"properties":{"pattern":{},"path":{}}}`),
		mkTool("edit", `{"type":"object","required":["filePath","oldString","newString"],"properties":{"filePath":{},"oldString":{},"newString":{}}}`),
		mkTool("todoread", `{"type":"object","properties":{}}`),
	}
	known := map[string]bool{}
	for _, x := range ts {
		known[x.Function.Name] = true
	}
	pa := primaryArgs(ts)
	if pa["bash"] != "command" || pa["read"] != "filePath" || pa["todoread"] != "" {
		t.Fatalf("primary args: %v", pa)
	}
	text := "Let me look.\n```bash\n$ git status --short --branch\n```\n```read\nsrc/main.go\n```\n```sh\ngo test ./...\n```\n```edit\n{\"filePath\":\"a.go\",\"oldString\":\"x\",\"newString\":\"y\"}\n```\n```go\nfunc main(){}\n```\n```todoread\n```"
	prose, calls := extractTaggedCalls(text, known, pa)
	want := []string{`bash {"command":"git status --short --branch"}`, `read {"filePath":"src/main.go"}`, `bash {"command":"go test ./..."}`, `edit {"filePath":"a.go","newString":"y","oldString":"x"}`, `todoread {}`}
	if len(calls) != len(want) {
		t.Fatalf("calls: %+v", calls)
	}
	for i, c := range calls {
		if got := c.Name + " " + string(c.Args); got != want[i] {
			t.Errorf("call %d = %s want %s", i, got, want[i])
		}
	}
	if !strings.Contains(prose, "func main(){}") || !strings.Contains(prose, "Let me look.") {
		t.Errorf("ordinary code block must survive as prose: %q", prose)
	}
	// End-to-end: a ```bash reply becomes an OpenAI tool_call.
	g := newFakeGraph(t, func(string) string { return "```bash\ngit status\n```" })
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[{"role":"user","content":"status?"}]}`)
	m := out.Choices[0].Message
	if *out.Choices[0].FinishReason != "tool_calls" || len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("tagged fence not turned into tool_call: %+v", out)
	}
}

func TestRepeatGuard(t *testing.T) {
	n := 0
	g := newFakeGraph(t, func(p string) string {
		n++
		if strings.Contains(p, "proposed the same command again") {
			return "On branch main, clean tree."
		}
		return "```bash\ngit status --short --branch\n```"
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	hist := `{"model":"m365-copilot",` + tools + `,"messages":[{"role":"user","content":"use git status"},
	 {"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"git status --short --branch\"}"}}]},
	 {"role":"tool","tool_call_id":"c1","content":"## main...origin/main"}]}`
	_, out := post(t, p.URL, hist)
	if n != 2 || *out.Choices[0].FinishReason != "stop" || string(out.Choices[0].Message.Content) != "On branch main, clean tree." {
		t.Fatalf("repeat guard: sends=%d out=%+v", n, out.Choices[0].Message)
	}
	// A different command is not a repeat.
	msgs := []oaiMessage{{Role: "assistant", ToolCalls: []oaiToolCall{{Function: oaiFunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}}}}, {Role: "tool", Content: "a"}}
	if repeatsLastCalls([]parsedCall{{Name: "bash", Args: json.RawMessage(`{"command":"ls -la"}`)}}, msgs) {
		t.Error("different args flagged as repeat")
	}
}
