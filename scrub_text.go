package main

// Copilot decorates answers with enterprise citations: inline [1] markers,
// "Sources:" / "References:" sections, "Learn more" link lists. A coding
// agent's output must not carry that. Stripped unless SOURCES=on.

import (
	"regexp"
	"strings"
)

var (
	// "[1]" / "[2][3]" only when preceded by whitespace or sentence punctuation —
	// never inside identifiers like arr[0].
	inlineCiteWS = regexp.MustCompile(`\s+\[\d{1,2}\](?:\[\d{1,2}\])*`)         // "done [1][2]" → "done"
	inlineCiteP  = regexp.MustCompile(`([.,;:!?)])\[\d{1,2}\](?:\[\d{1,2}\])*`) // "done.[1]" → "done."
	sourcesBlock = regexp.MustCompile(`(?is)\n+\s*(?:\*\*|#+\s*|__)?\s*(?:sources?|references?|citations?|learn more|related (?:links|documents?))\s*:?\s*(?:\*\*|__)?\s*:?\s*\n(?:\s*(?:[-*•]|\d+[.)])\s.*(?:\n|$))+\s*$`)
	attribLine   = regexp.MustCompile(`(?im)^\s*(?:source|from|via)\s*:\s*https?://\S+\s*$\n?`)
)

func stripCitations(s string) string {
	// Don't touch tool_call blocks — a "[1]" could be a legitimate index.
	parts := fencedCall.Split(s, -1)
	blocks := fencedCall.FindAllString(s, -1)
	for i := range parts {
		p := parts[i]
		p = sourcesBlock.ReplaceAllString(p, "\n")
		p = attribLine.ReplaceAllString(p, "")
		p = inlineCiteWS.ReplaceAllString(p, "")
		p = inlineCiteP.ReplaceAllString(p, "$1")
		parts[i] = p
	}
	var sb strings.Builder
	for i, p := range parts {
		sb.WriteString(p)
		if i < len(blocks) {
			sb.WriteString(blocks[i])
		}
	}
	return strings.TrimSpace(sb.String())
}
