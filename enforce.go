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

// hallucinationTells catch the opposite failure: the model confidently
// "inspected" something it never received — usually its own sandbox
// (/mnt/data) — or reports on files without any tool result behind it.
var hallucinationTells = regexp.MustCompile(`(?i)` +
	`/mnt/data|\bsandbox\b|code interpreter|\bmy environment\b|the files? (?:you )?(?:uploaded|provided|shared)` +
	`|\bi(?:'ve| have)? (?:inspected|listed|scanned|examined|explored|checked|looked (?:at|through|into)|reviewed|analy[sz]ed|searched|opened|read)\b[^.\n]{0,80}\b(?:file|files|filesystem|directory|folder|repo|repository|codebase|project|tree|structure)` +
	`|\b(?:the )?(?:current |working )?directory (?:is|appears|seems|contains|has)\b` +
	`|\b(?:there are|i (?:see|found|don'?t see|couldn'?t find)) (?:no|\d+|several|some) (?:files|folders|directories|entries)\b` +
	`|\b(?:no|empty) (?:files|repository|repo|directory|folder) (?:were |was |is )?(?:found|available|present|detected)\b` +
	`|\bunable to (?:locate|find|detect) (?:a |the |any )?(?:repository|repo|project|files)\b` +
	`|\bfrom (?:the )?(?:records|our (?:chat|messages|conversation|exchange|thread))\b|\benterprise (?:sources?|search|data)\b` +
	`|\b(?:in|from|via|searched|searching) (?:teams|sharepoint|onedrive|outlook|your (?:mailbox|email|inbox|calendar))\b` +
	`|\b(?:based on|according to) (?:the )?(?:teams|chat|meeting|email) (?:messages?|history|transcripts?|threads?)\b` +
	`|\bcannot (?:determine|tell|know|confirm) [^.\n]{0,60}\b(?:from|without) (?:the )?(?:records|chat|conversation|messages)\b`)

const enforceNudge = `STOP. You DO have tools and you MUST use them. You are not allowed to ask the user for files, paths, or command output, and you are not allowed to describe what you would do — do it.

You have NO sandbox and NO /mnt/data here. Anything you "inspected" without a tool result in this conversation was imagined. The developer's repository is on their machine; look at it with the tools (e.g. glob/list/read/bash) — nothing else counts.

Emit the tool_call block(s) now, e.g.:

` + "```tool_call" + `
{"name": "<tool>", "arguments": { ... }}
` + "```" + `

Output only tool_call blocks in this reply.`

// taskShaped: a first message that cannot be answered honestly without
// looking at the repo. Used only on fresh conversations.
var taskShaped = regexp.MustCompile(`(?i)\b(?:repo|repository|codebase|project|code|file|files|directory|folder|function|class|module|test|tests|bug|error|build|compile|implement|refactor|fix|add|change|update|explain|what does|what is this|how does|where is|this|here)\b`)

// firstTurnNeedsNudge: fresh conversation + tools offered + task-shaped ask
// + no tool call = the model answered from nothing.
func firstTurnNeedsNudge(userMsg string, toolsOffered bool, calls int) bool {
	return toolsOffered && calls == 0 && len(strings.TrimSpace(userMsg)) > 0 && taskShaped.MatchString(userMsg)
}

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
	return refusalTells.MatchString(r) || hallucinationTells.MatchString(r)
}
