package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGraph stands in for graph.microsoft.com: creates a conversation and
// echoes back a canned Copilot reply that includes the prompt it received.
func fakeGraph(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TESTTOKEN" {
			http.Error(w, "no bearer", 401)
			return
		}
		switch {
		case r.URL.Path == "/copilot/conversations":
			w.Write([]byte(`{"id":"conv-1"}`))
		case r.URL.Path == "/copilot/conversations/conv-1/chat":
			var gr graphChatRequest
			json.NewDecoder(r.Body).Decode(&gr)
			resp := graphChatResponse{ID: "conv-1", Messages: []graphMessage{
				{ID: "m1", Text: gr.Message.Text},
				{ID: "m2", Text: "ECHO:" + gr.Message.Text + "|SYS:" + firstCtx(gr)},
			}}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
}

func firstCtx(gr graphChatRequest) string {
	if len(gr.Contexts) > 0 {
		return gr.Contexts[0].Text
	}
	return ""
}

func newTestServer(t *testing.T, graphURL string) *httptest.Server {
	cfg := loadConfig()
	cfg.GraphBase = graphURL
	cfg.ClientID = "test"
	auth := &Authenticator{cfg: cfg, http: http.DefaultClient,
		tok: &tokenSet{AccessToken: "TESTTOKEN", ExpiresAt: time.Now().Add(time.Hour)}}
	s := &Server{cfg: cfg, graph: &GraphClient{cfg: cfg, auth: auth, http: http.DefaultClient}}
	return httptest.NewServer(s.routes())
}

const body = `{"model":"m365-copilot","stream":%v,"messages":[
 {"role":"system","content":"You are terse."},
 {"role":"user","content":[{"type":"text","text":"hi"}]},
 {"role":"assistant","content":"hello"},
 {"role":"user","content":"write fizzbuzz"}]}`

func TestNonStreaming(t *testing.T) {
	g := fakeGraph(t)
	defer g.Close()
	p := newTestServer(t, g.URL)
	defer p.Close()
	resp, err := http.Post(p.URL+"/v1/chat/completions", "application/json", strings.NewReader(strings.Replace(body, "%v", "false", 1)))
	if err != nil {
		t.Fatal(err)
	}
	var out oaiResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out.Object != "chat.completion" || len(out.Choices) != 1 {
		t.Fatalf("bad response: %d %+v", resp.StatusCode, out)
	}
	got := string(out.Choices[0].Message.Content)
	for _, want := range []string{"ECHO:Conversation so far:", "User: hi", "Assistant: hello", "write fizzbuzz", "SYS:You are terse."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if *out.Choices[0].FinishReason != "stop" || out.Usage == nil {
		t.Errorf("finish/usage wrong: %+v", out)
	}
}

func TestStreaming(t *testing.T) {
	g := fakeGraph(t)
	defer g.Close()
	p := newTestServer(t, g.URL)
	defer p.Close()
	resp, err := http.Post(p.URL+"/v1/chat/completions", "application/json", strings.NewReader(strings.Replace(body, "%v", "true", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var text strings.Builder
	var done bool
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
		if c.Object != "chat.completion.chunk" {
			t.Fatalf("object %q", c.Object)
		}
		if c.Choices[0].Delta != nil {
			text.WriteString(string(c.Choices[0].Delta.Content))
		}
	}
	if !done || !strings.Contains(text.String(), "write fizzbuzz") {
		t.Fatalf("stream done=%v text=%q", done, text.String())
	}
}
