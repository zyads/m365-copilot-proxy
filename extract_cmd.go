package main

// Command extraction fallback. Field-tested: Copilot refuses the *capability*
// ("I can't access the repo from this environment") but happily writes the
// exact command it wants run — "Run: git status --short --branch … and paste
// the output". When tools are offered, no tool_call was emitted, a shell tool
// exists, and the reply contains exactly one obvious shell command, turn that
// command into a real bash tool_call. Its natural behaviour becomes the action.

import (
	"regexp"
	"strings"
)

var (
	runLine    = regexp.MustCompile("(?im)^[ \\t]*(?:run|execute|try|use)[ \\t]*:?[ \\t]*`?([a-z][^`\\n]{2,200}?)`?[ \\t]*$")
	runNext    = regexp.MustCompile("(?im)\\b(?:run|execute|try)(?: this| the following| it)?[ \\t]*:[ \\t]*\\n\\s*\\n?[ \\t]*`?([a-z][^`\\n]{2,200}?)`?[ \\t]*$")
	shellFence = regexp.MustCompile("(?s)```(?:bash|sh|shell|zsh|console|)[ \\t]*\\n(.*?)\\n[ \\t]*```")
	inlineCmd  = regexp.MustCompile("(?i)\\brun\\s+`([^`\\n]{3,200})`")
	// Its own sandbox attempts: "`git status` failed with:", "I ran git log …",
	// "Running `go test`", "executed git status --short".
	attempted  = regexp.MustCompile("(?im)^[ \\t]*`?([a-z][^`\\n]{2,200}?)`?[ \\t]+(?:failed|errored|returned|exited|produced)(?: with)?[ \\t]*:|(?i)\\b(?:i (?:ran|executed|tried|attempted)|running|executing|executed)[ \\t]+`?([a-z][^`\\n]{2,120}?)`?(?:[ \\t]+(?:and|but|which|in|from|,|\\.|:)|$)")
	shellStart = regexp.MustCompile(`^(?:\$ )?(?:git|ls|cat|grep|rg|find|go|npm|pnpm|yarn|bun|node|python3?|pip|make|bazel|cargo|rustc|docker|kubectl|gh|curl|sed|awk|head|tail|wc|tree|pwd|cd|echo|test|pytest|mvn|gradle|dotnet|bash|sh)\b`)
)

// extractShellCommand returns a single command if the reply clearly proposes
// one for the developer to run, else "".
func extractShellCommand(reply string) string {
	var cands []string
	for _, m := range shellFence.FindAllStringSubmatch(reply, -1) {
		body := strings.TrimSpace(m[1])
		lines := strings.Split(body, "\n")
		if len(lines) > 3 {
			continue // a script, not a command
		}
		var cmds []string
		for _, l := range lines {
			l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "$ "))
			if l != "" && !strings.HasPrefix(l, "#") {
				cmds = append(cmds, l)
			}
		}
		if len(cmds) > 0 {
			cands = append(cands, strings.Join(cmds, " && "))
		}
	}
	for _, m := range runLine.FindAllStringSubmatch(reply, -1) {
		cands = append(cands, strings.TrimSpace(m[1]))
	}
	for _, m := range runNext.FindAllStringSubmatch(reply, -1) {
		cands = append(cands, strings.TrimSpace(m[1]))
	}
	for _, m := range inlineCmd.FindAllStringSubmatch(reply, -1) {
		cands = append(cands, strings.TrimSpace(m[1]))
	}
	for _, m := range attempted.FindAllStringSubmatch(reply, -1) {
		for _, g := range m[1:] {
			if g != "" {
				cands = append(cands, strings.TrimSpace(g))
			}
		}
	}
	// Keep only things that look like shell, dedupe.
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		c = strings.TrimSpace(strings.TrimPrefix(c, "$ "))
		if !shellStart.MatchString(c) || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) != 1 {
		return "" // ambiguous → let enforcement handle it
	}
	return out[0]
}

// shellToolName finds the client's shell tool, if any.
func shellToolName(known map[string]bool) string {
	for _, n := range []string{"bash", "shell", "execute_command", "run_command", "terminal", "exec"} {
		if known[n] {
			return n
		}
	}
	return ""
}
