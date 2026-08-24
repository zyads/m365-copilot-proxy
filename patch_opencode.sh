#!/usr/bin/env bash
# patch_opencode.sh — point a cloned OpenCode at the local M365 Copilot proxy.
#
# You almost certainly DON'T need this: drop ./opencode.json into your project
# (or ~/.config/opencode/opencode.json) and OpenCode uses the proxy as a
# provider with zero source changes. This script exists for the case where you
# truly want a hard fork with no escape hatch to OpenAI/GitHub Copilot.
#
# Handles both:
#   * sst/opencode          (TypeScript, current)
#   * opencode-ai/opencode  (Go, archived)
#
# Usage: ./patch_opencode.sh /path/to/opencode-clone [http://localhost:8080/v1]
set -euo pipefail

REPO="${1:?usage: $0 /path/to/opencode [proxy-base-url]}"
PROXY="${2:-http://localhost:8080/v1}"

[ -d "$REPO" ] || { echo "not a directory: $REPO" >&2; exit 1; }

# Every upstream base URL OpenCode is known to embed. Add to this list if a
# grep after patching still shows a stray host.
UPSTREAMS=(
  "https://api.openai.com/v1"
  "https://api.openai.com"
  "https://api.githubcopilot.com"
  "https://api.individual.githubcopilot.com"
  "https://api.business.githubcopilot.com"
  "https://api.enterprise.githubcopilot.com"
  "https://copilot-proxy.githubusercontent.com"
)

FILES=$(grep -rIl --exclude-dir=node_modules --exclude-dir=.git --exclude-dir=dist \
        --include='*.ts' --include='*.tsx' --include='*.js' --include='*.go' --include='*.json' \
        -e 'openai\.com' -e 'githubcopilot\.com' -e 'copilot-proxy\.githubusercontent\.com' "$REPO" || true)

if [ -z "$FILES" ]; then
  echo "no hardcoded OpenAI/GitHub Copilot URLs found in $REPO — nothing to patch."
  echo "(That's normal for sst/opencode: use opencode.json instead.)"
  exit 0
fi

echo "== files to patch =="; echo "$FILES"; echo

for f in $FILES; do
  cp "$f" "$f.bak"                       # keep a backup next to every touched file
  for u in "${UPSTREAMS[@]}"; do
    # '|' delimiter because the URLs contain '/'
    sed -i "s|${u}|${PROXY}|g" "$f"
  done
done

echo "== remaining upstream references (should be empty) =="
grep -rIn --exclude-dir=node_modules --exclude-dir=.git --exclude='*.bak' \
     -e 'api\.openai\.com' -e 'githubcopilot\.com' "$REPO" || echo "(none)"

echo
echo "patched. rebuild:"
echo "  sst/opencode:   cd $REPO && bun install && bun run --cwd packages/opencode build"
echo "  Go opencode:    cd $REPO && go build -o opencode ."
echo "restore:          find $REPO -name '*.bak' -exec sh -c 'mv \"\$1\" \"\${1%.bak}\"' _ {} \;"
