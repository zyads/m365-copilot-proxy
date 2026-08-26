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
	`|\bcould not be (?:executed|run) (?:against|in|on)\b|\bpath is (?:unavailable|not available|missing)\b|\bcurrent directory is not a (?:git )?repo` +
	`|\bfrom (?:the )?(?:records|our (?:chat|messages|conversation|exchange|thread))\b|\benterprise (?:sources?|search|data)\b` +
	`|\b(?:in|from|via|searched|searching) (?:teams|sharepoint|onedrive|outlook|your (?:mailbox|email|inbox|calendar))\b` +
	`|\b(?:based on|according to) (?:the )?(?:teams|chat|meeting|email) (?:messages?|history|transcripts?|threads?)\b` +
	`|\b(?:cannot|can'?t|unable to|no way to) (?:determine|tell|know|confirm|see|find|identify|reconstruct) [^.\n]{0,80}\b(?:from|without|using) (?:the |this |our )?(?:records|chat|conversation|messages|history|context|exchange)\b` +
	`|\b(?:chat|conversation|message) history (?:alone|only)\b|\bfrom (?:the |this )?(?:chat|conversation) (?:history|alone)\b` +
	// Asking the developer for something the tools can fetch.
	`|\b(?:give|provide|tell|share|send|paste|point) me (?:the |a |your )?(?:exact )?(?:repo(?:sitory)?|path|branch|directory|folder|file|url|name|location)\b` +
	`|\b(?:which|what) (?:repo(?:sitory)?|directory|folder|branch|path) (?:are you|is (?:it|this)|should i|do you)\b` +
	`|\b(?:once|if|when) you (?:give|provide|tell|share|send|point) me\b` +
	`|organization'?s? polic|access restrictions|learn\.microsoft\.com` +
	`|\bno (?:accessible |available |such )?[\w\-. ]{0,40}tools? (?:is |are )?(?:available|exposed|accessible|present|provided)\b` +
	`|\b(?:not|isn'?t|aren'?t) (?:available|exposed|accessible|provided|enabled) (?:in|for|to) (?:this|the current|my) (?:session|chat|conversation|context|toolset|environment)\b` +
	`|\btool(?:set)? (?:isn'?t|is not|aren'?t|are not) (?:exposed|available|accessible)\b`)

// enforceNudgeFor names the repo root when known — the model most often
// refuses because it has lost track of WHERE it is.
func enforceNudgeFor(root string) string {
	if root == "" {
		return enforceNudge
	}
	return enforceNudge + "\n\nThe repository is at " + root + " — that is the working directory. Do NOT ask for a path or branch. Call bash with `git status && git log --oneline -15` right now, then continue."
}

const enforceNudge = `STOP. You executed something yourself, or said you cannot access the repository. Neither is your job. Commands need the developer's approval, so you only PROPOSE them; the runner on the developer's machine executes approved commands and returns the output. Reply with ONLY the fenced command block(s) you propose, nothing else, for example:

` + "```bash" + `
git status --short --branch
` + "```" + `

The output will be pasted back to you. Do not explain, do not ask the developer for anything. You are not allowed to ask the user for files, paths, or command output, and you are not allowed to describe what you would do — do it.

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
