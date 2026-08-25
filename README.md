# m365-copilot-proxy

Turn Microsoft 365 Copilot into a full **agentic coding model** for OpenCode.

OpenCode talks OpenAI to `http://localhost:8080/v1`. The proxy signs you in
with device code, drives the Graph Copilot Chat API, and — the important part —
gives Copilot **tool calling** it doesn't natively have:

```
OpenCode  --- OpenAI JSON + tools[] --->  proxy  --- prompt + protocol --->  M365 Copilot
OpenCode  <--- OpenAI tool_calls -------  proxy  <--- text w/ tool_call blocks ---  Copilot
OpenCode runs read/grep/glob/edit/bash ON YOUR REPO, sends results back, repeat.
```

Copilot never touches your files; OpenCode's own local tools do. Copilot is
just the brain. That's how it "understands the repo" — the same way every
agent does: by reading it.

## MCP, skills, running commands — yes
All of that is OpenCode's side of the wire. MCP servers add tools to
`tools[]`; skills arrive as system-prompt text or a `skill` tool; `bash` is
just another tool. The proxy offers Copilot **whatever OpenCode sends**, and
OpenCode executes what Copilot picks — on your machine, under OpenCode's own
permission rules. Nothing to configure here. To keep turns small with many
MCP tools attached, the full catalog is sent once per Graph conversation and
later turns carry a names-only reminder.

## What's in the box
- **Tool-calling shim** — strict fenced-JSON protocol injected as a Copilot
  context; parsed back into real OpenAI `tool_calls` (parallel calls, lenient
  XML/wrapped forms, unknown tool names dropped, ordinary code fences untouched).
- **Conversation reuse** — the Graph conversation is kept per message-prefix;
  each agent step sends only the new tool results, not the whole transcript.
- **Streaming** — proper SSE incl. `tool_calls` deltas and `finish_reason`.
- **429/5xx backoff** with `Retry-After`.
- **Capability probe** — on startup, scans Graph `$metadata` for anything
  model/reasoning-shaped and logs it (see "Model selection").
- **Repo map** — branch, language mix, tree (depth 3), README head, injected
  as context so Copilot is oriented before its first tool call. Root from
  `REPO_ROOT` or parsed from the client's system prompt. `REPO_MAP=off` to disable.
- **Protocol enforcement** — if tools were offered and Copilot answers "I
  can't access your files…" or narrates instead of acting, the proxy re-prompts
  once on the same conversation. `ENFORCE_TOOLS=off` to disable.
- **Argument repair + validation** — trailing commas, single quotes, raw
  newlines, prose-wrapped JSON all get fixed; missing/unknown arguments (per
  the tool's schema) trigger one precise re-prompt ("missing required
  argument \"path\"") instead of a broken call reaching OpenCode.
- **Size control** — tool results are head+tail compacted (`MAX_TOOL_RESULT`,
  default 24k); replayed histories fold old tool results to one-liners; if
  Graph still says "too large" the proxy halves the budget and retries.
- **Per-conversation lock** — OpenCode's parallel requests (title gen + main
  turn) can't interleave writes to one Graph conversation.
- **`/stats`** — turns, tool calls, nudges, repairs, conv reuse, shrinks,
  latency p50/p95. Curl it while tuning against the real model.
- **Debug dump** — `DEBUG_DIR=/tmp/copilot-dbg` writes one JSON per turn:
  exact contexts + prompt Copilot saw, raw reply, whether a nudge fired.
  **Redacted by default** (emails, GUIDs, tokens, home dirs, tenant hosts,
  IPs); `DEBUG_RAW=1` keeps raw dumps for local use only. `scrub.sh` applies
  the same rules to any evidence file before it leaves the machine.
- **Always-on coding-agent persona** — Copilot shows up as a terse senior
  engineer that ignores your mailbox, not an Office add-in. Override with
  `AGENT_PERSONA="..."`, disable with `AGENT_PERSONA=off`.
- Confidential-client support (secret on refresh) and public-client device code.

## Model selection — read this
The Graph Copilot Chat API exposes **no model parameter and no model list**.
Microsoft picks the model server-side ("Think Deeper"/GPT-5 are UI features,
not API knobs). There is nothing to query, so this proxy doesn't pretend to.
What it does: the startup probe logs `POSSIBLE MODEL KNOBS` if Microsoft ever
adds one to the schema; then set `GRAPH_EXTRA_BODY='{"model":"..."}'` and every
chat body carries it. No code change.

## Install (persistent + self-updating)
```sh
curl -fsSL https://raw.githubusercontent.com/zyads/m365-copilot-proxy/main/install.sh | bash
opencode-copilot        # every time: ensures proxy is up + current, signs in once, launches OpenCode
```
`install.sh` builds, asks for tenant/client id once (stored 0600 in
`~/.config/m365-copilot-proxy/env`), installs a **user service** (systemd
with linger on Linux, launchd on macOS — survives logout, starts at boot;
nohup/setsid fallback elsewhere) and two commands:
- `opencode-copilot [args]` — start-if-down → `POST /update` (pull, rebuild,
  re-exec when idle if main moved) → show device code if not signed in → `exec opencode`.
- `copilot-proxy status|start|restart|stop|logs|update|auth`.

The proxy also checks origin/main itself on startup and every 6h
(`UPDATE_INTERVAL_MIN`, `AUTO_UPDATE=off`). A failed build rolls the checkout
back; the old binary keeps running.

## Visible thinking
OpenCode shows a collapsible thinking block for `reasoning_content` deltas.
Copilot has no reasoning tokens, so the proxy asks it to think inside
`<thinking>…</thinking>` first, splits that out, and streams it as
reasoning — the answer/tool calls stay clean. Planning before acting also
measurably improves protocol adherence. `THINKING=off` to disable.

## Entra app registration — what you actually need
- Delegated Graph permissions consented (you already have this).
- **Authentication → Advanced settings → Allow public client flows: Yes.**
  Device code flow needs this. No client secret. No application permissions.
- The signed-in user needs an M365 Copilot license.

## Run
```sh
export M365_TENANT_ID=<tenant-guid>
export M365_CLIENT_ID=<app-client-id>
./run.sh          # prints a microsoft.com/devicelogin URL + code on first run
```
Refresh token is cached at `~/.config/m365-copilot-proxy/token.json` (0600).

## Smoke test
```sh
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"m365-copilot","messages":[{"role":"user","content":"Summarise my last 3 emails"}]}'
```

## OpenCode
Copy `opencode.json` into the project you run OpenCode in (or
`~/.config/opencode/opencode.json`). Pick model `m365/m365-copilot`. No fork.

`patch_opencode.sh` rewrites hardcoded upstream URLs in a clone if you insist
on a hard fork.

## Known limits (honest)
- Tool calling is prompt-driven. A GPT-5-class Copilot follows the protocol
  reliably; if a reply comes back with prose instead of a block, OpenCode just
  shows the prose and you nudge it. Unknown tool names are dropped.
- Graph reports no token usage; `usage` is estimated (len/4).
- The Copilot Chat API is `/beta`; paths are env-overridable if Microsoft moves it.
- Copilot may refuse or add enterprise "Sources:" — sources are only appended
  to final prose answers, never to tool-call turns.
- Tenant Conditional Access can block device code; use `M365_CLIENT_SECRET`
  with a confidential app registration in that case (refresh still works).

## Dev
```sh
go test ./...   # mocked Graph: plain chat, full tool loop w/ conversation reuse, SSE tool deltas, lenient parsing
go build -o m365-copilot-proxy .
```
