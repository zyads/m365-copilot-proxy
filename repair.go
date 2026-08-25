package main

// Argument repair + validation. Models emit almost-JSON: single quotes,
// trailing commas, raw newlines inside strings, missing required keys. We fix
// what can be fixed mechanically and report precisely what can't so the model
// can retry once with a real error message instead of OpenCode choking.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// repairJSON tries progressively more aggressive fixes until the text parses
// as a JSON object. Returns the canonical (compact) form.
func repairJSON(raw string) (json.RawMessage, bool) {
	try := func(s string) (json.RawMessage, bool) {
		var v map[string]any
		if json.Unmarshal([]byte(s), &v) == nil {
			b, _ := json.Marshal(v)
			return b, true
		}
		return nil, false
	}
	s := strings.TrimSpace(raw)
	if b, ok := try(s); ok {
		return b, true
	}
	// 1. Strip ```json fences the model may have nested inside the block.
	s = strings.TrimPrefix(strings.TrimSuffix(s, "```"), "```json")
	s = strings.TrimSpace(s)
	// 2. Trailing commas before } or ].
	s = trailingComma.ReplaceAllString(s, "$1")
	if b, ok := try(s); ok {
		return b, true
	}
	// 3. Raw newlines/tabs inside string literals → escaped.
	if b, ok := try(escapeRawControlChars(s)); ok {
		return b, true
	}
	// 4. Single-quoted strings → double-quoted (only when no double quotes exist,
	//    otherwise we'd wreck legitimate apostrophes in content).
	if !strings.Contains(s, `"`) {
		if b, ok := try(strings.ReplaceAll(s, `'`, `"`)); ok {
			return b, true
		}
	}
	// 5. Grab the outermost {...} if the model wrapped it in prose.
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		if b, ok := try(escapeRawControlChars(trailingComma.ReplaceAllString(s[i:j+1], "$1"))); ok {
			return b, true
		}
	}
	return nil, false
}

var trailingComma = regexp.MustCompile(`,\s*([}\]])`)

// escapeRawControlChars walks the text and escapes literal newlines/tabs that
// occur inside double-quoted strings.
func escapeRawControlChars(s string) string {
	var sb strings.Builder
	inStr, esc := false, false
	for _, r := range s {
		switch {
		case esc:
			esc = false
			sb.WriteRune(r)
		case r == '\\' && inStr:
			esc = true
			sb.WriteRune(r)
		case r == '"':
			inStr = !inStr
			sb.WriteRune(r)
		case inStr && r == '\n':
			sb.WriteString(`\n`)
		case inStr && r == '\r':
			sb.WriteString(`\r`)
		case inStr && r == '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// toolSchema is the subset of JSON Schema we validate: required keys and
// top-level property names (to flag obvious typos like "filepath" vs "path").
type toolSchema struct {
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
	raw        json.RawMessage            // full schema, sent back on demand
}

func parseSchemas(tools []oaiTool) map[string]toolSchema {
	out := map[string]toolSchema{}
	for _, t := range tools {
		var s toolSchema
		_ = json.Unmarshal(t.Function.Parameters, &s)
		s.raw = t.Function.Parameters
		out[t.Function.Name] = s
	}
	return out
}

// validateArgs returns a human-readable problem list (empty = fine).
func validateArgs(name string, args json.RawMessage, schemas map[string]toolSchema) []string {
	sc, ok := schemas[name]
	if !ok {
		return nil
	}
	var got map[string]any
	if json.Unmarshal(args, &got) != nil {
		return []string{fmt.Sprintf("%s: arguments are not a JSON object", name)}
	}
	var problems []string
	for _, req := range sc.Required {
		if _, ok := got[req]; !ok {
			problems = append(problems, fmt.Sprintf("%s: missing required argument %q", name, req))
		}
	}
	if len(sc.Properties) > 0 {
		for k := range got {
			if _, ok := sc.Properties[k]; !ok {
				problems = append(problems, fmt.Sprintf("%s: unknown argument %q (valid: %s)", name, k, strings.Join(keys(sc.Properties), ", ")))
			}
		}
	}
	if len(problems) > 0 && len(sc.raw) > 0 {
		problems = append(problems, fmt.Sprintf("%s: exact schema: %s", name, compactJSON(sc.raw)))
	}
	return problems
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// repairNudge is the re-prompt sent when arguments could not be salvaged.
func repairNudge(problems []string) string {
	return "Your tool_call arguments were invalid:\n- " + strings.Join(problems, "\n- ") +
		"\n\nRe-emit the corrected tool_call block(s) now. Valid JSON only: double quotes, no trailing commas, newlines inside strings escaped as \\n. Output only tool_call blocks."
}
