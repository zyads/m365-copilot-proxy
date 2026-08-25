#!/usr/bin/env bash
# install.sh — one-shot setup: build, persistent user service, launcher.
#
#   curl -fsSL https://raw.githubusercontent.com/zyads/m365-copilot-proxy/main/install.sh | bash
#   (or from a clone: ./install.sh)
#
# After this:
#   opencode-copilot            # ensures proxy is running + current, signs in if needed, launches OpenCode
#   copilot-proxy status|restart|logs|update
#
# Idempotent. Re-run any time. The proxy self-updates from origin/main.
set -euo pipefail

REPO_URL="https://github.com/zyads/m365-copilot-proxy"
INSTALL_DIR="${M365_PROXY_DIR:-$HOME/.m365-copilot-proxy}"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
CONF_DIR="$HOME/.config/m365-copilot-proxy"
ENV_FILE="$CONF_DIR/env"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need git; need go; need curl

# --- checkout ---------------------------------------------------------------
if [ -d "$INSTALL_DIR/.git" ]; then
  git -C "$INSTALL_DIR" pull --ff-only --quiet origin main
elif [ -f "$(dirname "$0")/main.go" ] && [ "$(cd "$(dirname "$0")" && pwd)" != "$INSTALL_DIR" ]; then
  # Running from a clone somewhere else: mirror it into INSTALL_DIR so the
  # service has a stable path that self-update can pull into.
  git clone --quiet "$REPO_URL" "$INSTALL_DIR"
else
  [ -d "$INSTALL_DIR" ] || git clone --quiet "$REPO_URL" "$INSTALL_DIR"
fi
cd "$INSTALL_DIR"
echo "== building $(git rev-parse --short HEAD)"
go build -o m365-copilot-proxy .

# --- config -----------------------------------------------------------------
mkdir -p "$CONF_DIR" "$BIN_DIR"; chmod 700 "$CONF_DIR"
if [ ! -f "$ENV_FILE" ]; then
  echo "== first run: Entra app registration details (stored in $ENV_FILE, mode 600)"
  read -rp "M365_TENANT_ID: " tid
  read -rp "M365_CLIENT_ID: " cid
  read -rp "M365_CLIENT_SECRET (blank for public client): " csec
  echo "Auth mode: device_code (needs 'Allow public client flows'; often blocked by Conditional Access)"
  echo "           browser     (auth-code+PKCE via http://localhost:8080/auth/callback — register that redirect URI; CAP-friendly)"
  read -rp "AUTH_MODE [device_code/browser] (default device_code): " amode
  read -rp "Port (default 8080): " port; port="${port:-8080}"
  {
    echo "M365_TENANT_ID=$tid"
    echo "M365_CLIENT_ID=$cid"
    [ -n "$csec" ] && echo "M365_CLIENT_SECRET=$csec"
    [ -n "$amode" ] && echo "AUTH_MODE=$amode"
    echo "REPO_DIR=$INSTALL_DIR"
    echo "LISTEN=127.0.0.1:$port"
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
fi
grep -q '^REPO_DIR=' "$ENV_FILE" || echo "REPO_DIR=$INSTALL_DIR" >> "$ENV_FILE"
# Service managers start the proxy with a bare PATH; self-update needs go.
GO_BIN_DIR="$(dirname "$(command -v go)")"
if grep -q '^GO_BIN=' "$ENV_FILE"; then sed -i.bak "s#^GO_BIN=.*#GO_BIN=$GO_BIN_DIR#" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
else echo "GO_BIN=$GO_BIN_DIR" >> "$ENV_FILE"; fi
grep -q '^PATH=' "$ENV_FILE" || echo "PATH=$GO_BIN_DIR:$HOME/go/bin:/usr/local/bin:/usr/bin:/bin" >> "$ENV_FILE"

# --- launcher + control script ---------------------------------------------
install -m 755 "$INSTALL_DIR/bin/opencode-copilot" "$BIN_DIR/opencode-copilot"
install -m 755 "$INSTALL_DIR/bin/copilot-proxy"    "$BIN_DIR/copilot-proxy"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "!! add $BIN_DIR to your PATH";; esac

# --- mcp-autopilot plugin (works for every model, not just Copilot) --------
OC_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
mkdir -p "$OC_DIR/plugins"
cp "$INSTALL_DIR/opencode-plugin/mcp-autopilot.ts" "$OC_DIR/plugins/mcp-autopilot.ts"
[ -f "$OC_DIR/mcp-autopilot.json" ] || cp "$INSTALL_DIR/opencode-plugin/mcp-autopilot.example.json" "$OC_DIR/mcp-autopilot.json"

# --- persistent service -----------------------------------------------------
OS="$(uname -s)"
if [ "$OS" = Linux ] && command -v systemctl >/dev/null; then
  UNIT_DIR="$HOME/.config/systemd/user"; mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/m365-copilot-proxy.service" <<UNIT
[Unit]
Description=M365 Copilot -> OpenAI translation proxy
After=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$INSTALL_DIR/m365-copilot-proxy
WorkingDirectory=$INSTALL_DIR
Restart=always
RestartSec=2
# Self-update re-execs in place; exit 3 = "please restart me".
RestartForceExitStatus=3

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable --now m365-copilot-proxy.service
  systemctl --user restart m365-copilot-proxy.service
  # Keep running after logout / before login (needs no root on most distros).
  loginctl enable-linger "$USER" 2>/dev/null || echo "!! could not enable linger; service stops at logout (run: sudo loginctl enable-linger $USER)"
  echo "== systemd user service installed (journalctl --user -u m365-copilot-proxy -f)"
elif [ "$OS" = Darwin ]; then
  PLIST="$HOME/Library/LaunchAgents/com.zyads.m365-copilot-proxy.plist"
  mkdir -p "$HOME/Library/LaunchAgents" "$CONF_DIR/logs"
  # launchd can't read an env file; bake the vars in.
  ENV_XML=$(while IFS='=' read -r k v; do [ -n "$k" ] && printf '      <key>%s</key><string>%s</string>\n' "$k" "$v"; done < "$ENV_FILE")
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.zyads.m365-copilot-proxy</string>
  <key>ProgramArguments</key><array><string>$INSTALL_DIR/m365-copilot-proxy</string></array>
  <key>WorkingDirectory</key><string>$INSTALL_DIR</string>
  <key>EnvironmentVariables</key><dict>
$ENV_XML      <key>PATH</key><string>$(dirname "$(command -v go)"):/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$CONF_DIR/logs/proxy.log</string>
  <key>StandardErrorPath</key><string>$CONF_DIR/logs/proxy.log</string>
</dict></plist>
PLIST
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load -w "$PLIST"
  echo "== launchd agent installed (tail -f $CONF_DIR/logs/proxy.log)"
else
  echo "!! no service manager for $OS — the launcher will start the proxy with nohup/setsid instead"
fi

echo
echo "== done. run:  opencode-copilot"
