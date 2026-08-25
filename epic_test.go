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
