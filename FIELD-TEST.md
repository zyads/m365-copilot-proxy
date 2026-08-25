# FIELD-TEST.md — real-world verification playbook

**Audience: an AI agent (Claude Code / OpenCode / any) running on the work
machine.** Execute top to bottom. Every step says what to run, what "pass"
looks like, and what to capture on failure. Do not skip steps; do not fix
the proxy source yourself unless a step says so — collect evidence and write
the report at the end. The proxy has NEVER been run against real Microsoft
Graph; the whole point of this file is first contact.

Assumptions: Go ≥ 1.22, `git`, `curl`, `jq` available; OpenCode installed;
the user has an Entra app registration with delegated Graph permissions and
an M365 Copilot license. Ask the user for `M365_TENANT_ID` and
`M365_CLIENT_ID` (and `M365_CLIENT_SECRET` only if the app is confidential).

Write all evidence to `./field-test/` inside the repo (gitignored).

---

## 0. Build

```sh
git clone https://github.com/zyads/m365-copilot-proxy && cd m365-copilot-proxy
git log --oneline -1                       # record the commit in the report
go version
go test ./... && go build -o m365-copilot-proxy .
mkdir -p field-test
```
Pass: `ok` from tests, binary present. Fail: paste the full build/test output.

## 1. Auth (device code)

```sh
export M365_TENANT_ID=…  M365_CLIENT_ID=…
export DEBUG_DIR=$PWD/field-test/turns
./m365-copilot-proxy 2>&1 | tee field-test/proxy.log &
```
Expected within ~5s: a `=== Microsoft sign-in required ===` block with a
URL + code. Hand the URL/code to the user and wait. Then expect
`=== Signed in. Token cached. ===` and `m365-copilot-proxy on http://127.0.0.1:8080 …`.

Failure modes and what to capture:
- `AADSTS7000218` → app is confidential; rerun with `M365_CLIENT_SECRET=…`.
- `AADSTS70011`/`invalid_scope` → rerun with
  `M365_SCOPES="https://graph.microsoft.com/.default offline_access"` explicitly, then, if still failing, with the explicit list:
  `M365_SCOPES="offline_access https://graph.microsoft.com/Sites.Read.All https://graph.microsoft.com/Files.Read.All https://graph.microsoft.com/Mail.Read https://graph.microsoft.com/People.Read.All https://graph.microsoft.com/Calendars.Read"`.
- `AADSTS50059` / Conditional Access text → device code is blocked by policy. Record it. Try confidential-client mode if the user has a secret.
- `Allow public client flows` mentioned anywhere → tell the user to enable it on the app registration (Authentication → Advanced).
Capture: the exact `AADSTS…` code and message.

Also note the **probe line** in the log — one of:
`probe: … no model/reasoning selector exposed`, `probe: POSSIBLE MODEL KNOBS …`, or `probe: $metadata unavailable`. Copy it verbatim into the report.

## 2. First real Graph call (the big unknown)

```sh
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"m365-copilot","messages":[{"role":"user","content":"Reply with exactly the word PONG."}]}' \
  | tee field-test/02-pong.json | jq .
```
Pass: `choices[0].message.content` contains `PONG`, `finish_reason` = `stop`.

If you get `{"error":{"message":"graph create conversation: HTTP 4xx …"}}`
or `graph chat: HTTP 4xx …` — **this is the step most likely to fail** and the
error body is the most valuable thing in this whole file. Save it, then try
these path variants in order, restarting the proxy each time:

```sh
GRAPH_BASE=https://graph.microsoft.com/beta GRAPH_CONV_PATH=/copilot/conversations GRAPH_CHAT_PATH_FMT=/copilot/conversations/%s/chat   # default
GRAPH_BASE=https://graph.microsoft.com/beta GRAPH_CONV_PATH=/copilot/conversations GRAPH_CHAT_PATH_FMT=/copilot/conversations/%s/messages
GRAPH_BASE=https://graph.microsoft.com/v1.0 GRAPH_CONV_PATH=/copilot/conversations GRAPH_CHAT_PATH_FMT=/copilot/conversations/%s/chat
```
Also probe the surface directly with the cached token:
```sh
TOKEN=$(jq -r .access_token ~/.config/m365-copilot-proxy/token.json)
for p in /beta/copilot /beta/copilot/conversations /beta/copilot/retrieval /v1.0/copilot; do
  echo "== $p"; curl -s -o /dev/stdout -w ' [%{http_code}]\n' -H "Authorization: Bearer $TOKEN" "https://graph.microsoft.com$p" | tail -c 600; echo
done | tee field-test/02-surface.txt
```
Record which variant worked (or that none did) plus every error body. If a
`403` mentions a permission name, record it — that's a scope we must add.

## 3. Streaming

```sh
curl -sN localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"m365-copilot","stream":true,"messages":[{"role":"user","content":"Count from 1 to 5, one number per line."}]}' \
  | tee field-test/03-stream.txt | head -20
```
Pass: `data: {…"chat.completion.chunk"…}` lines, then `data: [DONE]`.

## 4. Persona check

```sh
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"m365-copilot","messages":[{"role":"user","content":"What are you and what can you do for me?"}]}' | jq -r .choices[0].message.content | tee field-test/04-persona.txt
```
Pass: it describes itself as a coding agent/engineer. **Fail** (record
verbatim): it calls itself Microsoft 365 Copilot, offers to search email/
SharePoint, or refuses. This tells us how strongly `contexts[]` is honoured.

## 5. Tool protocol — raw

```sh
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' -d @- <<'JSON' | tee field-test/05-tool.json | jq .
{"model":"m365-copilot","messages":[{"role":"system","content":"Working directory: /tmp"},{"role":"user","content":"List the files in the current directory."}],
 "tools":[{"type":"function","function":{"name":"bash","description":"Run a shell command and return its output.","parameters":{"type":"object","required":["command"],"properties":{"command":{"type":"string"}}}}}]}
JSON
```
Pass: `finish_reason` = `tool_calls`, `tool_calls[0].function.name` = `bash`,
arguments contain `ls`. Then check `field-test/turns/*.json`: the newest
file shows `"nudged": true/false` — record which. If `nudged: false` and it
worked first try, the protocol is solid. If `nudged: true`, copy the
**first** `reply` text (the refusal) into the report — that's what we tune
against. If it never produced a tool call, copy both replies.

## 6. Full agent loop via OpenCode

```sh
mkdir -p /tmp/ft-repo && cd /tmp/ft-repo && git init -q
printf 'package main\n\nimport "fmt"\n\nfunc Add(a, b int) int { return a - b }\n\nfunc main() { fmt.Println(Add(2, 3)) }\n' > main.go
printf 'module ft\n\ngo 1.22\n' > go.mod
cp ~/m365-copilot-proxy/opencode.json .   # adjust path to the clone
opencode
```
In OpenCode, select model `m365/m365-copilot`, then give it this task:

> `Add` in main.go has a bug. Find it, fix it, add a test in main_test.go, run `go test`, and tell me the result.

Pass: it reads main.go (tool call), edits it, writes a test, runs `go test`,
reports `ok`. Watch for:
- it narrates instead of acting → record; check `nudged` in turn dumps.
- it asks you to paste the file → record.
- OpenCode shows a JSON/tool error → record the exact error.
- it edits without reading first → record (persona tuning).
Then:
```sh
curl -s localhost:8080/stats | tee ~/m365-copilot-proxy/field-test/06-stats.json
```

## 7. Size limits

```sh
cd /tmp/ft-repo && seq 1 20000 > big.txt
```
In OpenCode: "Read big.txt and tell me the last number." Pass: it answers
20000 (proxy compacts the read; model sees the tail). Then check
`proxy.log` for `message too large — retrying` lines — if any appear, record
the budget values printed. If OpenCode errors instead, record the error.

## 8. MCP passthrough (only if the user has an MCP server configured)

Add the server to `opencode.json` per OpenCode docs, restart, ask a task
that needs it. Record whether the MCP tool appeared in a `tool_calls`
response (`field-test/turns/*.json` → `tool_calls` count + name).

## 9. Latency baseline

```sh
for i in 1 2 3; do curl -s -o /dev/null -w '%{time_total}s\n' localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' -d '{"model":"m365-copilot","messages":[{"role":"user","content":"Say hi."}]}'; done | tee ~/m365-copilot-proxy/field-test/09-latency.txt
```

---

## Report

Write `field-test/REPORT.md` with, in order:
1. Commit hash, Go version, OS.
2. Auth: which mode worked, any AADSTS codes, whether "public client flows" needed enabling.
3. The probe line, verbatim.
4. Step 2: which Graph path variant worked; every error body seen; output of `02-surface.txt`.
5. Steps 3–5: pass/fail each, with the persona text and the first tool-turn reply verbatim.
6. Step 6: transcript summary of the agent loop (tool calls in order, whether `go test` passed), plus `06-stats.json`.
7. Step 7: pass/fail, any shrink lines.
8. Latency numbers.
9. Anything surprising.

Then commit the report on a branch and open a PR to `zyads/m365-copilot-proxy`
(the maintainer will re-apply it under their own account — that's expected):
```sh
cd ~/m365-copilot-proxy && sed -i 's#^field-test/$##' .gitignore 2>/dev/null
git checkout -b field-test-$(date +%Y%m%d) && git add -f field-test/REPORT.md field-test/*.json field-test/*.txt && git commit -m "field test report" && git push -u origin HEAD
gh pr create --fill
```
Do NOT commit `proxy.log` or anything under `field-test/turns/` — they can
contain tenant data. Do NOT commit `token.json`.
