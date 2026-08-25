package main

// Visible thinking. OpenCode renders a collapsible "thinking" block for any
// reasoning the provider streams as `reasoning_content`. Copilot exposes no
// reasoning tokens, so we ask it to think inside <thinking>…</thinking> at
// the top of every reply, split that out, and stream it as reasoning. The
// side effect is the real win: a model that plans before acting follows the
// tool protocol far better. THINKING=off disables.

import (
	"regexp"
	"strings"
)

const thinkingInstruction = `## Thinking
Begin EVERY reply with a <thinking> block: what you know, what you still need to find out, which tool(s) to call and why, or — if answering — how you verified. Keep it tight (a few lines; more only for genuinely hard problems). Close it with </thinking>. Everything after it is your actual reply. Never put tool_call blocks inside <thinking>.`

var (
	thinkingRe    = regexp.MustCompile(`(?s)<thinking>\s*(.*?)\s*</thinking>`)
	unclosedRe    = regexp.MustCompile(`(?s)^\s*<thinking>\s*(.*)$`)
	thinkingFence = regexp.MustCompile("(?s)```thinking\\s*\\n(.*?)\\n\\s*```")
)

// splitThinking returns (reasoning, remainder). Handles a closed block, a
// ```thinking fence (some models fence everything), and an unclosed opener
// with no other content (model ran out — treat it all as reasoning).
func splitThinking(text string) (string, string) {
	if m := thinkingRe.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(thinkingRe.ReplaceAllString(text, ""))
	}
	if m := thinkingFence.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(thinkingFence.ReplaceAllString(text, ""))
	}
	if m := unclosedRe.FindStringSubmatch(text); m != nil && !strings.Contains(m[1], "```tool_call") {
		return strings.TrimSpace(m[1]), ""
	}
	return "", text
}
