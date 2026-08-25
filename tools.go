package main

// Tool-calling shim.
//
// Microsoft 365 Copilot cannot emit OpenAI `tool_calls`. It can emit text, and
// text is enough: we teach it a strict protocol — "to use a tool, reply with a
// ```tool_call fenced JSON block" — parse those blocks out of its answer, and
// hand them to OpenCode as real tool_calls. OpenCode executes them locally
// (read/grep/glob/edit/bash on YOUR repo), returns role:"tool" results, we
// render those back into the next prompt. That is the whole agent loop.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// toolProtocol is injected as a Copilot context whenever the request carries
// tools. It is deliberately blunt: strong models follow blunt protocols.
const toolProtocol = `You are operating as an autonomous coding agent inside a developer's terminal.
You have NO direct access to their files or shell. The ONLY way to see or change anything is to call the tools listed below — the client executes them on the developer's machine and sends you the results.

CRITICAL: You do NOT have a sandbox, a code interpreter, a Python environment, or a /mnt/data directory in this session. Any built-in "analyze files" or "run code" ability you think you have is NOT connected to the developer's machine and must not be used. If you have not received a tool result for a file in THIS conversation, you have not seen that file. "The repository" / "this project" / "the current directory" always means the developer's repo on their machine, reachable only through the tools below.

## Tool-call protocol (MANDATORY)
To call a tool, output a fenced block exactly like this:

` + "```tool_call" + `
{"name": "<tool name>", "arguments": { ...JSON matching the tool's parameters... }}
` + "```" + `

Rules:
1. One JSON object per block. Emit several blocks to call several tools at once.
2. The JSON must be valid — double quotes, no trailing commas, no comments.
3. When you call tools, output ONLY tool_call blocks (a one-line note before them is fine). Do NOT guess results. Stop and wait — results arrive in the next message.
4. Never invent file contents, paths, or command output. If you have not read it via a tool, you do not know it.
5. Before editing, read the file. Before claiming something works, run it or its tests.
6. When the task is complete, answer in plain prose with NO tool_call block. That signals you are done.
7. Think through the problem carefully before acting; prefer a few precise tool calls over many speculative ones.

## Available tools
`

const maxToolDescription = 400 // MCP servers ship essays; the model needs the gist

// renderToolCatalog produces the compact tool listing appended to the
// protocol. Sent in full only when a Graph conversation is created — Copilot
// keeps conversation state, so later turns get renderToolReminder instead.
func renderToolCatalog(tools []oaiTool) string {
	var sb strings.Builder
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		desc := strings.TrimSpace(t.Function.Description)
		if len(desc) > maxToolDescription {
			desc = desc[:maxToolDescription] + "…"
		}
		fmt.Fprintf(&sb, "### %s\n%s\n", t.Function.Name, desc)
		if len(t.Function.Parameters) > 0 {
			fmt.Fprintf(&sb, "Parameters (JSON Schema): %s\n", compactJSON(t.Function.Parameters))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// renderToolReminder is the per-turn context on a reused conversation: the
// protocol rules plus tool names only. Keeps every turn small even with
// dozens of MCP tools attached.
func renderToolReminder(tools []oaiTool) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Type == "function" {
			names = append(names, t.Function.Name)
		}
	}
	return "Tool-call protocol still applies (```tool_call fenced JSON, one object per block; plain prose when done). " +
		"Tools available (definitions were given earlier in this conversation): " + strings.Join(names, ", ")
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// Accept the fenced form and, leniently, an XML form some models drift into.
var (
	fencedCall = regexp.MustCompile("(?s)```(?:tool_call|tool|json\\s+tool_call)\\s*\\n(.*?)\\n\\s*```")
	xmlCall    = regexp.MustCompile("(?s)<tool_call>\\s*(.*?)\\s*</tool_call>")
)

type parsedCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"arguments"`
}

// extractToolCalls pulls every protocol block out of Copilot's reply. It
// returns the surviving prose (blocks removed), the calls in order, and any
// problems (unparseable blocks, unknown tools, invalid arguments) worth
// re-prompting about. `repaired` counts blocks that only parsed after repair.
func extractToolCalls(text string, known map[string]bool) (prose string, calls []parsedCall) {
	prose, calls, _, _ = extractToolCallsChecked(text, known, nil)
	return
}

func extractToolCallsChecked(text string, known map[string]bool, schemas map[string]toolSchema) (prose string, calls []parsedCall, problems []string, repaired int) {
	prose = text
	for _, re := range []*regexp.Regexp{fencedCall, xmlCall} {
		for _, m := range re.FindAllStringSubmatch(prose, -1) {
			raw := strings.TrimSpace(m[1])
			var pc parsedCall
			if err := json.Unmarshal([]byte(raw), &pc); err != nil || pc.Name == "" {
				// Some models wrap as {"tool_call": {...}} — unwrap once.
				var wrap struct {
					ToolCall *parsedCall `json:"tool_call"`
				}
				if json.Unmarshal([]byte(raw), &wrap) == nil && wrap.ToolCall != nil {
					pc = *wrap.ToolCall
				} else if fixed, ok := repairJSON(raw); ok {
					_ = json.Unmarshal(fixed, &pc)
					if pc.Name != "" {
						repaired++
					}
				}
			}
			if pc.Name == "" {
				problems = append(problems, "a tool_call block was not valid JSON: "+truncate([]byte(raw), 160))
				continue
			}
			if known != nil && !known[pc.Name] {
				problems = append(problems, fmt.Sprintf("unknown tool %q", pc.Name))
				continue
			}
			if len(pc.Args) == 0 || string(pc.Args) == "null" {
				pc.Args = json.RawMessage("{}")
			} else if !json.Valid(pc.Args) {
				if fixed, ok := repairJSON(string(pc.Args)); ok {
					pc.Args = fixed
					repaired++
				} else {
					problems = append(problems, fmt.Sprintf("%s: arguments are not valid JSON", pc.Name))
					continue
				}
			}
			if ps := validateArgs(pc.Name, pc.Args, schemas); len(ps) > 0 {
				problems = append(problems, ps...)
				continue
			}
			calls = append(calls, pc)
		}
		prose = re.ReplaceAllString(prose, "")
	}
	return strings.TrimSpace(prose), calls, problems, repaired
}

// toOpenAIToolCalls converts parsed calls into the wire shape, minting ids.
func toOpenAIToolCalls(calls []parsedCall, seed string) []oaiToolCall {
	out := make([]oaiToolCall, 0, len(calls))
	for i, c := range calls {
		out = append(out, oaiToolCall{
			Index: i,
			ID:    fmt.Sprintf("call_%s_%d", seed, i),
			Type:  "function",
			Function: oaiFunctionCall{
				Name:      c.Name,
				Arguments: string(c.Args),
			},
		})
	}
	return out
}
