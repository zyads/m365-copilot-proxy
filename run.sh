#!/usr/bin/env bash
# run.sh — start the proxy. Edit the two IDs once; everything else is optional.
set -euo pipefail
cd "$(dirname "$0")"

export M365_TENANT_ID="${M365_TENANT_ID:?set M365_TENANT_ID (tenant GUID)}"
export M365_CLIENT_ID="${M365_CLIENT_ID:?set M365_CLIENT_ID (app registration client id)}"
# Optional overrides:
#   AUTH_MODE=device_code|client_credentials   (default device_code — the one that works)
#   M365_SCOPES="https://graph.microsoft.com/.default offline_access"
#   AGENT_PERSONA="custom text" | off      (default: built-in coding-agent persona)
#   REPO_ROOT=/path/to/repo  REPO_MAP=off  ENFORCE_TOOLS=off  DEBUG_DIR=/tmp/copilot-dbg
#   LISTEN=127.0.0.1:8080  MODEL_NAME=m365-copilot  M365_TIMEZONE=America/New_York
#   GRAPH_BASE=https://graph.microsoft.com/beta
#   GRAPH_CONV_PATH=/copilot/conversations  GRAPH_CHAT_PATH_FMT=/copilot/conversations/%s/chat

[ -x ./m365-copilot-proxy ] || go build -o m365-copilot-proxy .
exec ./m365-copilot-proxy
