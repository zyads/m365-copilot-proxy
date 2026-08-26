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
	"sort"
	"strings"
)

// toolProtocol is injected as a Copilot context whenever the request carries
// tools. It is deliberately blunt: strong models follow blunt protocols.
const toolProtocol = `You are the planning brain of an automated coding agent. You do NOT run anything yourself — do not use any built-in code execution, Python, or sandbox you may have; they are not connected to the developer's machine and will only mislead you. A RUNNER on the developer's machine executes what you write and pastes the output back as the next message.

## How to make the runner do something (MANDATORY format)
Write a fenced block whose language tag is the runner command name and whose body is the argument:

` + "```bash" + `
git status --short --branch
` + "```" + `

` + "```read" + `
src/main.go
` + "```" + `

` + "```grep" + `
TODO
` + "```" + `

Runner commands with several arguments take JSON instead:

` + "```edit" + `
{"filePath": "src/main.go", "oldString": "a - b", "newString": "a + b"}
` + "```" + `

Rules:
1. One block per command. Several blocks = several commands at once.
2. When you write blocks, write ONLY blocks (one short line before them is fine). Then STOP — the output arrives in the next message. Never invent output.
3. Do not ask the developer to run anything or paste anything. Do not say you "cannot access" the repository — you were never asked to; write the block and the runner will.
4. Read before editing; run the build/tests before claiming success.
5. When the task is DONE, answer in plain prose with NO fenced command blocks (use inline code instead). A fenced bash block in a reply means "run this now".
6. Think briefly before acting; a few precise commands beat many speculative ones.
7. Resuming ("continue", "where we left off"): FIRST write bash blocks for git status and git log -20, read AGENTS.md / TODO / NOTES / plan docs, then continue. Chat history, Teams, mail and "records" are never the source of truth for repo state.
8. If a todowrite/todoread runner command exists: on any task with 3+ steps, write the plan to it first and keep it updated.

## Runner commands available (tag = name; body = the argument named first, or JSON for several)
`

const (
	maxToolDescription  = 400 // core tools
	maxMCPDescription   = 140 // one-liners in the grouped MCP listing
	groupMinSize        = 2   // a shared "<server>_" prefix with ≥2 tools is an MCP server
	maxFullSchemaTools  = 24  // above this many tools, only core tools keep schemas
	reminderNamesPerSrv = 8   // names shown per server in standing orders
)

// toolGroups splits tools into core (no server prefix) and MCP servers keyed
// by prefix (OpenCode names MCP tools "<server>_<tool>").
type toolGroups struct {
	core    []oaiTool
	servers map[string][]oaiTool
	order   []string // server names, sorted
}

func groupTools(tools []oaiTool) toolGroups {
	count := map[string]int{}
	for _, t := range tools {
		if i := strings.Index(t.Function.Name, "_"); i > 0 {
			count[t.Function.Name[:i]]++
		}
	}
	g := toolGroups{servers: map[string][]oaiTool{}}
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		if i := strings.Index(t.Function.Name, "_"); i > 0 && count[t.Function.Name[:i]] >= groupMinSize {
			g.servers[t.Function.Name[:i]] = append(g.servers[t.Function.Name[:i]], t)
			continue
		}
		g.core = append(g.core, t)
	}
	for k := range g.servers {
		g.order = append(g.order, k)
	}
	sort.Strings(g.order)
	return g
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func renderToolFull(t oaiTool, descMax int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "### %s\n%s\n", t.Function.Name, clip(t.Function.Description, descMax))
	if pa := primaryArg(t.Function.Parameters); pa != "" {
		fmt.Fprintf(&sb, "Block body = %q. ", pa)
	}
	if len(t.Function.Parameters) > 0 {
		fmt.Fprintf(&sb, "Parameters (JSON Schema): %s\n", compactJSON(t.Function.Parameters))
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderToolCatalog scales from 5 tools to 500. Core tools always get full
// schemas. MCP tools are grouped by server as one-liners (no schemas) unless
// the whole catalog is small; the exact schema comes back on demand — when a
// call has bad/missing arguments (see repairNudge) or the user names the
// tool/server (see renderToolReminder).
func renderToolCatalog(tools []oaiTool) string {
	g := groupTools(tools)
	var sb strings.Builder
	full := len(tools) <= maxFullSchemaTools
	if len(g.core) > 0 {
		sb.WriteString("### Core tools (full schemas)\n\n")
		for _, t := range g.core {
			sb.WriteString(renderToolFull(t, maxToolDescription))
		}
	}
	if len(g.order) > 0 {
		fmt.Fprintf(&sb, "### MCP server tools (%d servers, %d tools) — call by EXACT name\n", len(g.order), len(tools)-len(g.core))
		if !full {
			sb.WriteString("Schemas omitted for brevity: pass the obvious arguments from the description; if required arguments are missing you will receive the exact schema and can retry.\n")
		}
		sb.WriteString("\n")
		for _, srv := range g.order {
			fmt.Fprintf(&sb, "#### %s (%d)\n", srv, len(g.servers[srv]))
			for _, t := range g.servers[srv] {
				if full {
					sb.WriteString(renderToolFull(t, maxToolDescription))
				} else {
					fmt.Fprintf(&sb, "- %s: %s\n", t.Function.Name, clip(t.Function.Description, maxMCPDescription))
				}
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// primaryArg picks the argument a bare fenced body maps to: the first
// required property, else the first property. "" if the tool takes none.
func primaryArg(params json.RawMessage) string {
	var sc struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(params, &sc) != nil {
		return ""
	}
	if len(sc.Required) > 0 {
		return sc.Required[0]
	}
	for _, pref := range []string{"command", "filePath", "path", "pattern", "query", "content", "text"} {
		if _, ok := sc.Properties[pref]; ok {
			return pref
		}
	}
	return ""
}

// fence aliases: what models write → what OpenCode calls it.
var fenceAlias = map[string]string{"sh": "bash", "shell": "bash", "zsh": "bash", "console": "bash", "terminal": "bash", "cat": "read", "file": "read", "search": "grep", "rg": "grep", "find": "glob", "ls": "list"}

var taggedFence = regexp.MustCompile("(?s)```[ \\t]*([A-Za-z][A-Za-z0-9_\\-.]*)[ \\t]*\\n(.*?)\\n?[ \\t]*```")

// extractTaggedCalls: ```<tool>\nbody``` → call. Body that is a JSON object
// becomes the arguments; otherwise it becomes the tool's primary argument.
func extractTaggedCalls(text string, known map[string]bool, primary map[string]string) (prose string, calls []parsedCall) {
	prose = text
	for _, m := range taggedFence.FindAllStringSubmatch(text, -1) {
		tag := strings.ToLower(m[1])
		if a, ok := fenceAlias[tag]; ok {
			tag = a
		}
		name := ""
		if known[tag] {
			name = tag
		} else if r := resolveToolName(tag, known); r != "" && strings.EqualFold(squash(r), squash(tag)) {
			name = r
		}
		if name == "" {
			continue // ordinary code block (go, json, python …)
		}
		body := strings.TrimSpace(m[2])
		body = strings.TrimPrefix(body, "$ ")
		var args json.RawMessage
		if strings.HasPrefix(body, "{") {
			if fixed, ok := repairJSON(body); ok {
				args = fixed
			}
		}
		if args == nil {
			pa := primary[name]
			if pa == "" {
				args = json.RawMessage("{}")
			} else {
				b, _ := json.Marshal(map[string]string{pa: body})
				args = b
			}
		}
		calls = append(calls, parsedCall{Name: name, Args: args})
		prose = strings.Replace(prose, m[0], "", 1)
	}
	return strings.TrimSpace(prose), calls
}

// renderToolReminder is the per-turn "standing orders" on a reused
// conversation. Field-tested: Copilot's server-side persona reasserts over
// turns and it forgets where it is, so every turn restates who it is, the
// sandbox/M365 ban, the protocol, the tools, and the working directory.
// With hundreds of MCP tools the list is summarised per server; tools or
// servers the user just NAMED get their full definitions included.
func renderToolReminder(tools []oaiTool, root, userMsg string) string {
	g := groupTools(tools)
	var sb strings.Builder
	sb.WriteString("=== STANDING ORDERS (every turn) ===\n")
	sb.WriteString("You are an autonomous coding agent in the developer's terminal. No sandbox, no /mnt/data, no Teams/mail/SharePoint/\"records\" — the repo on the developer's machine is the only source of truth, reachable ONLY via tools.\n")
	if root != "" {
		sb.WriteString("Working directory (the repo): " + root + " — never ask for a path or branch; look with git status / ls / read.\n")
	}
	sb.WriteString("You do not run anything yourself (no built-in code execution / sandbox). To act, write fenced blocks tagged with the runner command name, body = argument, e.g. ```bash\ngit status\n``` or ```read\npath/to/file\n``` (JSON body for several arguments). Then STOP; the output comes back next message. Never ask the developer to run or paste anything. Plain prose with no fenced command blocks only when the task is DONE. Begin with a brief <thinking>…</thinking>.\n")
	names := make([]string, 0, len(g.core))
	for _, t := range g.core {
		names = append(names, t.Function.Name)
	}
	sb.WriteString("Runner commands: " + strings.Join(names, ", ") + "\n")
	if len(g.order) > 0 {
		sb.WriteString("MCP servers (tools are named <server>_<tool>; full list was given at the start of this conversation):\n")
		for _, srv := range g.order {
			ts := g.servers[srv]
			var few []string
			for i, t := range ts {
				if i >= reminderNamesPerSrv {
					few = append(few, fmt.Sprintf("… +%d more", len(ts)-i))
					break
				}
				few = append(few, strings.TrimPrefix(t.Function.Name, srv+"_"))
			}
			fmt.Fprintf(&sb, "  - %s (%d): %s\n", srv, len(ts), strings.Join(few, ", "))
		}
	}
	// Anything the user named this turn: full definitions, so it cannot claim
	// the tool "isn't exposed".
	if userMsg != "" {
		known := map[string]bool{}
		for _, t := range tools {
			known[t.Function.Name] = true
		}
		if hit := mentionedTools(userMsg, known); len(hit) > 0 {
			sb.WriteString("The developer referred to these tools — they ARE available; use them:\n")
			byName := map[string]oaiTool{}
			for _, t := range tools {
				byName[t.Function.Name] = t
			}
			for i, n := range hit {
				if i >= 12 {
					fmt.Fprintf(&sb, "… +%d more\n", len(hit)-i)
					break
				}
				sb.WriteString(renderToolFull(byName[n], maxToolDescription))
			}
		}
	}
	sb.WriteString("=== END STANDING ORDERS ===")
	return sb.String()
}

// Accept the fenced form and, leniently, an XML form some models drift into.
var (
	// The protocol form, plus what models actually write: ```json fences,
	// bare fences, "tool_call" as a label line, XML, and a naked JSON
	// object on its own line. All must contain "name" and "arguments".
	fencedCall = regexp.MustCompile("(?s)```[ \\t]*(?:tool_call|tool|json[ \\t]*tool_call|json|javascript|)?[ \\t]*\\n(\\s*\\{.*?\\})\\s*\\n?\\s*```")
	xmlCall    = regexp.MustCompile("(?s)<tool_call>\\s*(.*?)\\s*</tool_call>")
	labelCall  = regexp.MustCompile("(?im)^[ \\t]*(?:tool_call|tool call|call|action)[ \\t]*:[ \\t]*(\\{.*\\})[ \\t]*$")
	bareCall   = regexp.MustCompile("(?m)^[ \\t]*(\\{[^\\n]*\"name\"[^\\n]*\"arguments\"[^\\n]*\\})[ \\t]*$|^[ \\t]*(\\{[^\\n]*\"arguments\"[^\\n]*\"name\"[^\\n]*\\})[ \\t]*$")
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
	for _, re := range []*regexp.Regexp{fencedCall, xmlCall, labelCall, bareCall} {
		for _, m := range re.FindAllStringSubmatch(prose, -1) {
			raw := strings.TrimSpace(m[1])
			if raw == "" && len(m) > 2 {
				raw = strings.TrimSpace(m[2])
			}
			// A fenced block that is ordinary JSON/code (no tool shape) is not a call.
			if !strings.Contains(raw, "\"name\"") && !strings.Contains(raw, "'name'") && !strings.Contains(raw, "\"tool_call\"") {
				continue
			}
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
				if real := resolveToolName(pc.Name, known); real != "" {
					pc.Name = real
					repaired++
				} else {
					problems = append(problems, fmt.Sprintf("unknown tool %q (valid: %s)", pc.Name, strings.Join(sortedKeys(known), ", ")))
					continue
				}
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

// resolveToolName maps a mangled name to a real one: case/punctuation-
// insensitive match first ("local-memory.search" → "local-memory_search"),
// then a unique suffix match ("search" → "local-memory_search" if only one
// tool ends that way). Returns "" if ambiguous or nothing fits.
func resolveToolName(name string, known map[string]bool) string {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	n := norm(name)
	if n == "" {
		return ""
	}
	var hit string
	for k := range known {
		if norm(k) == n {
			return k
		}
	}
	for k := range known {
		if strings.HasSuffix(norm(k), n) || strings.HasSuffix(n, norm(k)) {
			if hit != "" {
				return "" // ambiguous
			}
			hit = k
		}
	}
	return hit
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mentionedTools returns known tool names (or MCP server prefixes) that the
// user's text explicitly refers to — "use local-memory", "localmemory mcp",
// "call todoread". Matching is punctuation-insensitive on both sides.
func mentionedTools(userMsg string, known map[string]bool) []string {
	low := squash(userMsg)
	var out []string
	seen := map[string]bool{}
	for k := range known {
		lk := strings.ToLower(k)
		prefix := lk
		if i := strings.Index(lk, "_"); i > 2 {
			prefix = lk[:i]
		}
		if nameIn(low, lk) || (len(prefix) >= 4 && nameIn(low, prefix)) {
			if !seen[k] {
				out = append(out, k)
				seen[k] = true
			}
		}
	}
	sort.Strings(out)
	return out
}

// nameIn: does the (squashed) text mention name, written either joined
// ("localmemory") or with the joiners as spaces ("local memory")?
func nameIn(low, name string) bool {
	joined := squash(name)
	spaced := strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' || r == '.' }), " ")
	return wordIn(low, joined) || (spaced != joined && wordIn(low, spaced))
}

// squash lowercases and collapses punctuation so "local-memory", "local_memory"
// and "localmemory" all compare equal, while word boundaries survive as spaces.
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			// joiners inside a name: drop
		default:
			b.WriteRune(' ')
		}
	}
	return b.String()
}

// wordIn: needle appears in hay bounded by non-alphanumerics (so "read"
// doesn't match inside "todoread").
func wordIn(hay, needle string) bool {
	isAl := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }
	for i := strings.Index(hay, needle); i >= 0; {
		j := i + len(needle)
		if (i == 0 || !isAl(hay[i-1])) && (j == len(hay) || !isAl(hay[j])) {
			return true
		}
		k := strings.Index(hay[i+1:], needle)
		if k < 0 {
			return false
		}
		i += 1 + k
	}
	return false
}

func mentionNudge(names []string) string {
	return "The developer explicitly asked you to use: " + strings.Join(names, ", ") + ". You did not call any of them. Call the appropriate one NOW with a ```tool_call block. Do not explain, do not ask — call it."
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
