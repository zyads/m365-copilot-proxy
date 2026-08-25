package main

// Protocol enforcement. The #1 way prompt-driven tool calling dies is the
// model answering "I don't have access to your files" or describing what it
// *would* do instead of emitting a tool_call block. When tools are offered and
// the reply has none, we check for those tells and re-prompt once on the same
// conversation with a blunt reminder. One retry, never more — if it still
// refuses, the user sees the prose and can steer.

import (
	"regexp"
	"strings"
)

var refusalTells = regexp.MustCompile(`(?i)` +
	`\b(?:don'?t|do not|cannot|can'?t|unable to|not able to|no way to)\s+(?:directly\s+)?(?:have\s+)?(?:access|see|view|read|open|browse|run|execute|inspect|look at)\b` +
	`|\bno (?:direct )?access to\b` +
	`|\bas an ai\b` +
	`|\bi (?:would|'d|will|'ll) (?:need|have) to (?:see|read|look at|open|run)\b` +
	`|\b(?:please|could you) (?:share|paste|provide|upload) (?:the|your)\b` +
	`|\byou (?:can|could|should|would) run\b` +
	`|\bhere'?s (?:what|how) (?:i|you) (?:would|could|can)\b`)

const enforceNudge = `STOP. You DO have tools and you MUST use them. You are not allowed to ask the user for files, paths, or command output, and you are not allowed to describe what you would do — do it.

Emit the tool_call block(s) now, e.g.:

` + "```tool_call" + `
{"name": "<tool>", "arguments": { ... }}
` + "```" + `

Output only tool_call blocks in this reply.`

// needsNudge reports whether a tool-less reply looks like a refusal or a
// narrated non-action when tools were available.
func needsNudge(reply string, toolsOffered bool, calls int) bool {
	if !toolsOffered || calls > 0 {
		return false
	}
	r := strings.TrimSpace(reply)
	if r == "" {
		return true
	}
	return refusalTells.MatchString(r)
}
