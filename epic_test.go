package main

import (
	"encoding/json"
	"io"
	"net/http"
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
	if len(problems) != 3 || !strings.Contains(problems[0], `missing required argument "content"`) || !strings.Contains(problems[1], `missing required argument "path"`) || !strings.Contains(problems[2], `unknown argument "filepath"`) {
		t.Errorf("problems: %v", problems)
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
