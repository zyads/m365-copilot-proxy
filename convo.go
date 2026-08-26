package main

// Conversation reuse + prompt flattening.
//
// Copilot has one text message per turn, not a messages[] array. Naively we'd
// open a fresh conversation per request and replay the whole transcript — which
// for a 40-step agent loop means re-sending everything 40 times. Instead we
// remember which Graph conversation already holds a given message prefix and
// send only the delta.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

type convEntry struct {
	ID       string
	Consumed int // number of OpenAI messages already sent to this conversation
	Seen     time.Time
}

type ConvCache struct {
	mu  sync.Mutex
	m   map[string]convEntry // key: hash(messages[:n])
	ttl time.Duration
}

func NewConvCache(ttl time.Duration) *ConvCache {
	return &ConvCache{m: map[string]convEntry{}, ttl: ttl}
}

// prefixHash hashes a NORMALISED view of each message: role, trimmed text,
// tool-call name+canonical args, tool-result text. Field-tested: hashing the
// raw struct never matched, because the client echoes our assistant message
// back without reasoning_content and with re-serialised arguments, so every
// turn replayed the whole history into a fresh conversation.
func prefixHash(msgs []oaiMessage) string {
	h := sha256.New()
	for _, m := range msgs {
		h.Write([]byte(m.Role))
		h.Write([]byte{1})
		h.Write([]byte(strings.TrimSpace(string(m.Content))))
		h.Write([]byte{1})
		for _, tc := range m.ToolCalls {
			h.Write([]byte(tc.Function.Name))
			h.Write([]byte{2})
			h.Write([]byte(canonJSON(tc.Function.Arguments)))
			h.Write([]byte{2})
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Lookup finds the longest prefix of msgs that an existing conversation has
// already consumed. Returns ("",0) when nothing matches. Tries the longest
// prefix first; the common case (OpenCode appended an assistant turn plus
// tool results plus a new user message) matches at the first assistant turn
// we produced.
func (c *ConvCache) Lookup(msgs []oaiMessage) (id string, consumed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for n := len(msgs) - 1; n >= 1; n-- {
		e, ok := c.m[prefixHash(msgs[:n])]
		if !ok {
			continue
		}
		if now.Sub(e.Seen) > c.ttl {
			delete(c.m, prefixHash(msgs[:n]))
			continue
		}
		return e.ID, e.Consumed
	}
	return "", 0
}

// Remember records that conversation id now holds everything in msgs plus the
// assistant reply we just produced (so the next request's prefix matches).
func (c *ConvCache) Remember(id string, msgs []oaiMessage, reply oaiMessage) {
	full := append(append([]oaiMessage{}, msgs...), reply)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[prefixHash(full)] = convEntry{ID: id, Consumed: len(full), Seen: time.Now()}
	// Opportunistic GC.
	if len(c.m) > 512 {
		for k, e := range c.m {
			if time.Since(e.Seen) > c.ttl {
				delete(c.m, k)
			}
		}
	}
}

// splitSystem separates system/developer messages from the rest.
func splitSystem(msgs []oaiMessage) (system string, rest []oaiMessage) {
	var sys []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if t := strings.TrimSpace(string(m.Content)); t != "" {
				sys = append(sys, t)
			}
			continue
		}
		rest = append(rest, m)
	}
	return strings.Join(sys, "\n\n"), rest
}

// renderTurns renders a slice of non-system messages as the text Copilot
// receives. Tool calls and results are rendered explicitly so the model sees
// what it asked for and what came back.
//
// `all` is the full (non-system) history — used only to label tool results by
// name; `msgs` is the slice actually rendered (the whole history, or the delta).
func renderTurns(all, msgs []oaiMessage, isDelta bool, budget int) string {
	if !isDelta {
		msgs = foldHistory(msgs, keepVerbatimResults)
	}
	names := map[string]string{}
	for _, m := range all {
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Function.Name
		}
	}
	var sb strings.Builder
	if !isDelta && len(msgs) > 1 {
		sb.WriteString("Conversation so far:\n\n")
	}
	for i, m := range msgs {
		text := strings.TrimSpace(string(m.Content))
		last := i == len(msgs)-1
		switch m.Role {
		case "user":
			if last && !isDelta {
				sb.WriteString("\n\nNow respond to this:\n")
			} else if !last || len(msgs) > 1 {
				sb.WriteString("User: ")
			}
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case "assistant":
			sb.WriteString("Assistant: ")
			if text != "" {
				sb.WriteString(text)
				sb.WriteString("\n")
			}
			for _, tc := range m.ToolCalls {
				sb.WriteString("```tool_call\n{\"name\": \"" + tc.Function.Name + "\", \"arguments\": " + tc.Function.Arguments + "}\n```\n")
			}
			sb.WriteString("\n")
		case "tool":
			name := names[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			sb.WriteString("Tool result [" + name + "]:\n")
			sb.WriteString(compactResult(text, budget))
			sb.WriteString("\n\n")
		}
	}
	if isDelta {
		sb.WriteString("Continue. Call more tools if needed, otherwise give the final answer in prose.")
	}
	return strings.TrimSpace(sb.String())
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
