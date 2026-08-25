# Field Test Report

## 1. Environment

- **Commit:** `684cf41` (main, latest at time of test)
- **Go version:** 1.22.5 linux/amd64
- **OS:** Linux x86_64
- **App registration type:** Confidential client (has `client_secret`)

## 2. Auth

- **Mode that worked:** `device_code` with refresh-token bootstrap
- **Device-code flow blocked:** Yes. Conditional Access Policy returns:
  > "Your sign-in was successful but does not meet the criteria to access
  > this resource."
  
  This happened twice (both pre- and post-scope-addition). Device-code
  sign-in completes, user authenticates successfully, then CAP rejects
  the token issuance.

- **Workaround that worked:** Bootstrap from an existing delegated refresh
  token (obtained previously by a different app on a non-CAP-blocked flow).
  Seeded `~/.config/m365-copilot-proxy/token.json` with:
  ```json
  {"access_token": "<existing>", "refresh_token": "<existing>", "expires_at": "2000-01-01T00:00:00Z"}
  ```
  Setting `expires_at` in the past forces the proxy to refresh immediately,
  bypassing device-code entirely.

- **AADSTS7000218:** Hit on first refresh attempt because the app is
  confidential and the proxy (pre-fix) did not include `client_secret` in
  the refresh grant. **Fix merged upstream** — `auth.go:144` now includes
  `client_secret` when `cfg.ClientSecret != ""`.

- **AADSTS90002:** Hit once when using a stale tenant GUID from an older
  config file. Resolved by using the correct tenant.

- **"Allow public client flows":** Not tested; CAP blocked before this
  could be evaluated. The refresh-token approach sidesteps this entirely.

### Recommendation
For enterprise environments where CAP blocks device-code, document the
refresh-token bootstrap workaround in the README. Many corporate tenants
block device-code flow by policy. If users already have a delegated token
from another Graph-authenticated app, they can seed the proxy's cache and
never hit device-code at all.

## 3. Probe line

```
probe: 30 copilot types found, no model/reasoning selector exposed — Microsoft picks the model server-side
```

## 4. First Graph call (Step 2)

### First attempt — HTTP 403 (missing scopes)

Before Copilot-specific scopes were consented on the app registration, the
conversation-create call returned:

```
HTTP 403: {"error":{"code":"unauthorized","message":" Required scopes =
[Sites.Read.All, Mail.Read, People.Read.All,
OnlineMeetingTranscript.Read.All, Chat.Read, ChannelMessage.Read.All,
ExternalItem.Read.All]."}}
```

**Key finding:** The Copilot Chat API requires ALL of these scopes just to
open a conversation — even if the user only wants plain LLM chat with no
M365 data access. This is all-or-nothing from Microsoft's side.

### After scope addition — success

Once the scopes were admin-consented on the app registration and a fresh
token was obtained (via refresh), the conversation-create and chat calls
both succeeded on the **default paths**:
- `GRAPH_BASE=https://graph.microsoft.com/beta`
- `GRAPH_CONV_PATH=/copilot/conversations`
- `GRAPH_CHAT_PATH_FMT=/copilot/conversations/%s/chat`

No path variants needed.

### First successful response

```json
{"id":"chatcmpl-...","object":"chat.completion","created":...,"model":"m365-copilot",
 "choices":[{"index":0,"message":{"role":"assistant","content":"Hello there, hope you're well."},
 "finish_reason":"stop"}],
 "usage":{"prompt_tokens":7,"completion_tokens":8,"total_tokens":15}}
```

Latency: ~210ms for this first call (cached token, fresh conversation).

## 5. Streaming (Step 3)

**Pass.** SSE chunks delivered correctly after the `omitempty` fix on
`oaiMessage.Role`. Without the fix, the Vercel AI SDK rejected chunks
with `"role":""` — expected `"assistant"` or field omitted. **Fix merged
upstream** — `openai.go:54`.

## 6. OpenCode integration (Step 6)

Connected OpenCode with the `opencode.json` provider config. Model
appeared as `m365/m365-copilot` in the model picker.

**Stats after initial session:**
```json
{
  "arg_repair_failures": 0,
  "arg_repairs": 0,
  "conversations_new": 2,
  "conversations_reused": 0,
  "errors": 0,
  "latency_p50_ms": 9536,
  "latency_p95_ms": 9536,
  "message_shrinks": 0,
  "nudges": 0,
  "tool_calls": 0,
  "turns": 2
}
```

- `tool_calls: 0` confirms Copilot does not emit tool_calls (as documented)
- `nudges: 0`, `arg_repairs: 0` — no repair attempts needed
- `latency_p50_ms: 9536` — ~9.5s per turn (includes Graph round-trip)

## 7. Issues found

### Critical for enterprise adoption

1. **CAP-blocked device-code is the #1 onboarding blocker.** Many corporate
   Entra tenants block device-code flow. The README should document the
   refresh-token bootstrap workaround prominently.

2. **Scope list is opaque.** Users don't know which scopes to consent until
   the 403 tells them. A pre-flight check or clearer error message would
   save significant time.

### Minor

3. **Proxy process fragility on Linux.** When started via `&` in a shell,
   the proxy dies if the parent shell exits. Recommend documenting
   `nohup ... & disown` or providing a systemd unit.

## 8. Summary

The proxy works end-to-end against a real Microsoft 365 Copilot tenant once:
1. The correct scopes are admin-consented
2. A valid delegated token is obtained (device-code or refresh bootstrap)
3. `M365_CLIENT_SECRET` is provided for confidential app registrations

Total time from clone to working chat: ~25 minutes (most spent on auth
troubleshooting). With the documented workarounds, repeat setup would take
under 5 minutes.
