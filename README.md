# m365-copilot-proxy

Local OpenAI-compatible front door for Microsoft 365 Copilot. OpenCode (or curl)
talks to `http://localhost:8080/v1`; the proxy signs you in with device code,
opens a Graph Copilot conversation, sends your flattened chat, and hands the
answer back as OpenAI JSON (streaming or not).

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

## Known limits (not bugs)
- Copilot returns no `tool_calls`; OpenCode's agentic edit/run loop won't fire.
- Graph reports no token usage; `usage` is estimated (len/4).
- The Copilot Chat API is `/beta`; paths are env-overridable if Microsoft moves it.

## Dev
```sh
go test ./...   # mocked Graph, covers streaming + non-streaming + content-array form
go build -o m365-copilot-proxy .
```
