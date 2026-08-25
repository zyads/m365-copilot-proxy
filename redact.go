package main

// De-identification for anything that leaves the machine (DEBUG_DIR dumps,
// field-test evidence). Conservative regex pass: emails, UPNs, GUIDs
// (tenant/client/object ids), bearer/JWT tokens, home directories, tenant
// hostnames, IPs, and phone-ish numbers. Set DEBUG_RAW=1 to keep raw dumps
// locally — never commit those.

import (
	"regexp"
)

var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)bearer\s+[a-z0-9\-_\.=]+`), "Bearer [REDACTED-TOKEN]"},
	{regexp.MustCompile(`\beyJ[a-zA-Z0-9\-_]+\.[a-zA-Z0-9\-_]+\.[a-zA-Z0-9\-_]+\b`), "[REDACTED-JWT]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), "[REDACTED-EMAIL]"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "[REDACTED-GUID]"},
	{regexp.MustCompile(`(?i)\b[a-z0-9\-]+\.sharepoint\.com\b`), "[REDACTED-TENANT].sharepoint.com"},
	{regexp.MustCompile(`(?i)\b[a-z0-9\-]+\.onmicrosoft\.com\b`), "[REDACTED-TENANT].onmicrosoft.com"},
	{regexp.MustCompile(`/home/[^/\s"']+`), "/home/[USER]"},
	{regexp.MustCompile(`/Users/[^/\s"']+`), "/Users/[USER]"},
	{regexp.MustCompile(`(?i)[A-Z]:\\Users\\[^\\\s"']+`), `C:\Users\[USER]`},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "[REDACTED-IP]"},
	{regexp.MustCompile(`\+?\d[\d\s\-()]{8,}\d`), "[REDACTED-NUMBER]"},
}

func redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
