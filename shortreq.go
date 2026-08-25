package main

// Utility requests. OpenCode fires small side requests — "generate a
// conversation title", summaries — with no tools. They must not get the
// coding-agent persona or the <thinking> instruction (Copilot then answers
// "**Conversation title:** …" and the label leaks into the UI), and the reply
// must be a bare line.

import (
	"regexp"
	"strings"
)

var titleAsk = regexp.MustCompile(`(?i)\b(?:generate|create|write|produce|give)\b[^.\n]{0,60}\btitle\b|\btitle\b[^.\n]{0,40}\b(?:for|of) (?:this|the) (?:conversation|session|chat|thread)`)

// isUtilityRequest: no tools and the system/user text asks for a title.
func isUtilityRequest(system string, turns []oaiMessage, tools int) bool {
	if tools > 0 {
		return false
	}
	if titleAsk.MatchString(system) {
		return true
	}
	if len(turns) > 0 && titleAsk.MatchString(string(turns[len(turns)-1].Content)) {
		return true
	}
	return false
}

var (
	labelPrefix = regexp.MustCompile(`(?i)^\s*(?:\*\*|__)?\s*(?:conversation |session |chat |suggested |proposed )?title\s*(?:\*\*|__)?\s*[:\-–—]\s*`)
	wrapQuotes  = regexp.MustCompile("^[\"'`“”‘’]+|[\"'`“”‘’]+$")
)

// cleanShortAnswer turns "**Conversation title:** \"Friendly greeting.\"" into
// "Friendly greeting".
func cleanShortAnswer(s string) string {
	_, s = splitThinking(s)
	s = strings.TrimSpace(s)
	// Take the first non-empty line; titles are single lines.
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			s = t
			break
		}
	}
	s = labelPrefix.ReplaceAllString(s, "")
	s = strings.TrimSpace(strings.Trim(s, "*_"))
	s = wrapQuotes.ReplaceAllString(s, "")
	s = strings.TrimRight(strings.TrimSpace(s), ".")
	return s
}
