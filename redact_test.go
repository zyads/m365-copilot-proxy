package main

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := `user jane.doe@contoso.com tenant 12345678-abcd-4ef0-9abc-1234567890ab at https://contoso.sharepoint.com/sites/x
Authorization: Bearer eyJhbGciOi.eyJzdWIiOi.SflKxwRJ path /home/jdoe/work and /Users/jdoe/x and C:\Users\jdoe\y ip 10.1.2.3 phone +1 216 555 0100`
	out := redact(in)
	for _, leaked := range []string{"jane.doe", "contoso", "12345678-abcd", "eyJ", "jdoe", "10.1.2.3", "555 0100"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q in: %s", leaked, out)
		}
	}
	for _, want := range []string{"[REDACTED-EMAIL]", "[REDACTED-GUID]", "[REDACTED-TENANT].sharepoint.com", "[REDACTED-TOKEN]", "/home/[USER]", "/Users/[USER]", `C:\Users\[USER]`, "[REDACTED-IP]", "[REDACTED-NUMBER]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
	if !strings.Contains(redact("token=eyJhbGciOi.eyJzdWIiOi.SflKxwRJ"), "[REDACTED-JWT]") {
		t.Error("bare JWT not redacted")
	}
	if redact("func Add(a, b int) int { return a + b }") != "func Add(a, b int) int { return a + b }" {
		t.Error("ordinary code must survive")
	}
}
