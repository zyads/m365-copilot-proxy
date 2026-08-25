package main

// OpenAI wire types — only the fields OpenCode / the Vercel AI SDK actually use,
// but including the full tool-calling surface so agentic loops round-trip.

import (
	"encoding/json"
	"strings"
)

// oaiContent handles both `"content": "text"` and the array-of-parts form.
type oaiContent string

func (c *oaiContent) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*c = ""
		return nil
	}
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

type oaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string, per OpenAI spec
}

type oaiToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id"`
	Type     string          `json:"type"` // always "function"
	Function oaiFunctionCall `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role,omitempty"`
	Content    oaiContent    `json:"content"`
	Reasoning  string        `json:"reasoning_content,omitempty"` // DeepSeek-style; AI SDK renders as thinking
	Reasoning2 string        `json:"reasoning,omitempty"`         // newer openai-compatible field name
	Name       string        `json:"name,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"` // on role:"tool"
}

// oaiTool is the definition OpenCode sends for each of its local tools.
type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Stream   bool         `json:"stream"`
	Tools    []oaiTool    `json:"tools,omitempty"`
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

// approxTokens is a cheap estimate; Graph reports no token counts.
func approxTokens(s string) int { return (len(s) + 3) / 4 }
