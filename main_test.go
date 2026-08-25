package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGraph stands in for graph.microsoft.com. It records every prompt it
// receives (per conversation) and replies with whatever `reply` returns.
type fakeGraph struct {
	*httptest.Server
	mu      sync.Mutex
	convs   int
	prompts []string
	ctxs    [][]graphContext
	reply   func(prompt string) string
}

func newFakeGraph(t *testing.T, reply func(string) string) *fakeGraph {
	t.Helper()
	f := &fakeGraph{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TESTTOKEN" {
			http.Error(w, "no bearer", 401)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/copilot/conversations":
			f.convs++
			w.Write([]byte(`{"id":"conv-1"}`))
		case strings.HasSuffix(r.URL.Path, "/chat"):
			var body struct {
				Message  struct{ Text string } `json:"message"`
				Contexts []graphContext        `json:"contexts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.prompts = append(f.prompts, body.Message.Text)
			f.ctxs = append(f.ctxs, body.Contexts)
			json.NewEncoder(w).Encode(graphChatResponse{ID: "conv-1", Messages: []graphMessage{
				{ID: "m1", Text: body.Message.Text},
				{ID: "m2", Text: f.reply(body.Message.Text)},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	return f
}

func newProxy(t *testing.T, graphURL string) *httptest.Server {
	cfg := loadConfig()
	cfg.GraphBase = graphURL
	cfg.ClientID = "test"
	cfg.MaxRetries = 0
	auth := &Authenticator{cfg: cfg, http: http.DefaultClient,
		tok: &tokenSet{AccessToken: "TESTTOKEN", ExpiresAt: time.Now().Add(time.Hour)}}
	s := &Server{cfg: cfg, graph: &GraphClient{cfg: cfg, auth: auth, http: http.DefaultClient}, convs: NewConvCache(time.Hour)}
	return httptest.NewServer(s.routes())
}

func post(t *testing.T, url, body string) (*http.Response, oaiResponse) {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out oaiResponse
	if resp.Header.Get("Content-Type") == "application/json" {
		json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

const tools = `"tools":[{"type":"function","function":{"name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},
{"type":"function","function":{"name":"bash","description":"Run a shell command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]`

func TestPlainChat(t *testing.T) {
	g := newFakeGraph(t, func(p string) string { return "ECHO:" + p })
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	resp, out := post(t, p.URL, `{"model":"m365-copilot","messages":[
		{"role":"system","content":"You are terse."},
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":"hello"},
		{"role":"user","content":"write fizzbuzz"}]}`)
	if resp.StatusCode != 200 || out.Object != "chat.completion" {
		t.Fatalf("bad: %d %+v", resp.StatusCode, out)
	}
	got := string(out.Choices[0].Message.Content)
	for _, want := range []string{"User: hi", "Assistant: hello", "write fizzbuzz"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if len(g.ctxs[0]) != 2 || g.ctxs[0][0].Text != defaultPersona || g.ctxs[0][1].Text != "You are terse." {
		t.Errorf("contexts should be [persona, system]: %+v", g.ctxs[0])
	}
	if *out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish %v", *out.Choices[0].FinishReason)
	}
}

// The full agent loop: tools advertised → Copilot emits tool_call blocks →
// proxy returns OpenAI tool_calls → client sends results → proxy reuses the
// Graph conversation and sends ONLY the delta → Copilot answers in prose.
func TestToolLoopWithConversationReuse(t *testing.T) {
	g := newFakeGraph(t, func(p string) string {
		if strings.Contains(p, "Tool result [read]") {
			return "The file says hello. Done."
		}
		return "Let me look.\n```tool_call\n{\"name\":\"read\",\"arguments\":{\"path\":\"main.go\"}}\n```\n```tool_call\n{\"name\": \"bash\", \"arguments\": {\"command\": \"go test\"}}\n```\n```tool_call\n{\"name\":\"nope\",\"arguments\":{}}\n```"
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()

	// Turn 1
	_, out := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[
		{"role":"system","content":"sys"},{"role":"user","content":"what does main.go say?"}]}`)
	m := out.Choices[0].Message
	if *out.Choices[0].FinishReason != "tool_calls" || len(m.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool_calls (unknown 'nope' dropped), got %+v", out)
	}
	if m.ToolCalls[0].Function.Name != "read" || m.ToolCalls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("bad call 0: %+v", m.ToolCalls[0])
	}
	if string(m.Content) != "Let me look." {
		t.Errorf("prose should be stripped of blocks: %q", m.Content)
	}
	if len(g.ctxs[0]) != 3 || !strings.Contains(g.ctxs[0][2].Text, "### read") || !strings.Contains(g.ctxs[0][2].Text, "### bash") {
		t.Errorf("tool protocol context missing: %+v", g.ctxs[0])
	}

	// Turn 2: client echoes assistant msg + tool results + nothing else.
	tcJSON, _ := json.Marshal(m.ToolCalls)
	_, out2 := post(t, p.URL, `{"model":"m365-copilot",`+tools+`,"messages":[
		{"role":"system","content":"sys"},{"role":"user","content":"what does main.go say?"},
		{"role":"assistant","content":"Let me look.","tool_calls":`+string(tcJSON)+`},
		{"role":"tool","tool_call_id":"`+m.ToolCalls[0].ID+`","content":"package main // hello"},
		{"role":"tool","tool_call_id":"`+m.ToolCalls[1].ID+`","content":"ok"}]}`)
	if *out2.Choices[0].FinishReason != "stop" || string(out2.Choices[0].Message.Content) != "The file says hello. Done." {
		t.Fatalf("turn 2 wrong: %+v", out2)
	}
	if g.convs != 1 {
		t.Errorf("expected conversation reuse, created %d", g.convs)
	}
	if len(g.prompts) != 2 || strings.Contains(g.prompts[1], "what does main.go say?") || !strings.Contains(g.prompts[1], "Tool result [read]:\npackage main // hello") {
		t.Errorf("turn 2 should send only the delta, got: %q", g.prompts[1])
	}
}

func TestStreamingWithToolCalls(t *testing.T) {
	g := newFakeGraph(t, func(string) string {
		return "```tool_call\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n```"
	})
	defer g.Close()
	p := newProxy(t, g.URL)
	defer p.Close()
	resp, _ := post(t, p.URL, `{"model":"m365-copilot","stream":true,`+tools+`,"messages":[{"role":"user","content":"list files"}]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var sawTool, done bool
	var finish string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			break
		}
		var c oaiResponse
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if strings.Contains(payload, `"role":""`) {
			t.Fatalf("empty role serialised: %s", payload)
		}
		d := c.Choices[0].Delta
		if d != nil && len(d.ToolCalls) == 1 && d.ToolCalls[0].Function.Name == "bash" {
			sawTool = true
		}
		if c.Choices[0].FinishReason != nil {
			finish = *c.Choices[0].FinishReason
		}
	}
	if !done || !sawTool || finish != "tool_calls" {
		t.Fatalf("done=%v sawTool=%v finish=%q", done, sawTool, finish)
	}
}

func TestExtractLenientForms(t *testing.T) {
	known := map[string]bool{"read": true}
	_, calls := extractToolCalls("<tool_call>{\"name\":\"read\",\"arguments\":{\"path\":\"a\"}}</tool_call>", known)
	if len(calls) != 1 {
		t.Fatalf("xml form: %+v", calls)
	}
	_, calls = extractToolCalls("```tool_call\n{\"tool_call\":{\"name\":\"read\",\"arguments\":{\"path\":\"b\"}}}\n```", known)
	if len(calls) != 1 {
		t.Fatalf("wrapped form: %+v", calls)
	}
	prose, calls := extractToolCalls("just prose with ```go\nfmt.Println()\n``` code", known)
	if len(calls) != 0 || !strings.Contains(prose, "fmt.Println()") {
		t.Fatalf("ordinary code fences must survive: %q %+v", prose, calls)
	}
}
