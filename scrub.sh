#!/usr/bin/env bash
# scrub.sh — de-identify evidence files in place before they leave the machine.
# Same rules as the proxy's built-in redaction (redact.go), applied to any
# text file: emails, GUIDs, tokens, home dirs, tenant hostnames, IPs, numbers.
# Usage: ./scrub.sh field-test/REPORT.md field-test/*.json field-test/*.txt
set -euo pipefail
[ $# -gt 0 ] || { echo "usage: $0 <files…>" >&2; exit 1; }
for f in "$@"; do
  [ -f "$f" ] || continue
  perl -pi -e '
    s/\bBearer\s+[A-Za-z0-9\-_.=]+/Bearer [REDACTED-TOKEN]/gi;
    s/\beyJ[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+\b/[REDACTED-JWT]/g;
    s/[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}/[REDACTED-EMAIL]/g;
    s/\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b/[REDACTED-GUID]/g;
    s/\b[a-z0-9\-]+\.sharepoint\.com\b/[REDACTED-TENANT].sharepoint.com/gi;
    s/\b[a-z0-9\-]+\.onmicrosoft\.com\b/[REDACTED-TENANT].onmicrosoft.com/gi;
    s#/home/[^/\s"'"'"']+#/home/[USER]#g;
    s#/Users/[^/\s"'"'"']+#/Users/[USER]#g;
    s/[A-Za-z]:\\Users\\[^\\\s"'"'"']+/C:\\Users\\[USER]/g;
    s/\b(?:\d{1,3}\.){3}\d{1,3}\b/[REDACTED-IP]/g;
    s/\+?\d[\d\s\-()]{8,}\d/[REDACTED-NUMBER]/g;
  ' "$f"
  echo "scrubbed $f"
done
echo
echo "Now READ each file and remove by hand anything the rules cannot know:"
echo "  people's names, project/ticket/customer names, internal hostnames, anything from mail/Teams/SharePoint."
