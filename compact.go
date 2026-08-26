package main

// Size control. Copilot's per-message limit is not documented and not
// generous. Two defences:
//   * compactResult: head+tail truncation of a single tool result.
//   * foldHistory: when a whole transcript must be replayed into a fresh
//     conversation, old tool results collapse to one-line summaries and only
//     the most recent few stay verbatim.
// graph.go additionally retries with a halved budget when Graph rejects a
// message as too large.

import (
	"fmt"
	"strings"
)

const (
	defaultToolResultBudget = 24_000 // bytes of a single tool result kept verbatim
	keepVerbatimResults     = 6      // most recent tool results replayed in full
	foldedResultBytes       = 200    // bytes kept of a folded result
)

// compactResult keeps the first ~2/3 and last ~1/3 of an oversized result so
// the model sees both the beginning (headers, signatures) and the end (errors,
// summaries) of command output.
func compactResult(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	head := budget * 2 / 3
	tail := budget - head
	// Snap to line boundaries so we don't cut mid-token.
	if i := strings.LastIndex(s[:head], "\n"); i > head/2 {
		head = i
	}
	tailStart := len(s) - tail
	if i := strings.Index(s[tailStart:], "\n"); i >= 0 && i < tail/2 {
		tailStart += i + 1
	}
	return s[:head] + fmt.Sprintf("\n\n…[%d bytes omitted — use a narrower read/grep if you need this part]…\n\n", tailStart-head) + s[tailStart:]
}

// foldHistory returns a copy of msgs where all but the last `keep` tool
// results are collapsed to a short summary line.
func foldHistory(msgs []oaiMessage, keep int) []oaiMessage {
	var idx []int
	for i, m := range msgs {
		if m.Role == "tool" {
			idx = append(idx, i)
		}
	}
	if len(idx) <= keep {
		return msgs
	}
	out := make([]oaiMessage, len(msgs))
	copy(out, msgs)
	for _, i := range idx[:len(idx)-keep] {
		t := string(out[i].Content)
		lines := strings.Count(t, "\n") + 1
		snippet := strings.TrimSpace(t)
		if len(snippet) > foldedResultBytes {
			snippet = snippet[:foldedResultBytes] + "…"
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ⏎ ")
		out[i].Content = oaiContent(fmt.Sprintf("[folded: %d bytes, %d lines] %s", len(t), lines, snippet))
	}
	return out
}

// capText keeps head+tail of an oversized block with a marker in between.
func capText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max * 3 / 4
	tail := max - head
	if i := strings.LastIndex(s[:head], "\n"); i > head/2 {
		head = i
	}
	return s[:head] + "\n\n[... " + itoa(len(s)-head-tail) + " bytes of application instructions omitted for length ...]\n\n" + s[len(s)-tail:]
}
