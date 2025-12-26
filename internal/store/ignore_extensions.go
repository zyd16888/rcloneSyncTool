package store

import "strings"

// ParseIgnoreExtensions parses rule.ignore_extensions into a list of normalized suffixes.
// Supported inputs: ".png .jpg", "png,jpg", "*.png".
// Unsupported glob patterns (containing wildcard chars) are ignored.
func ParseIgnoreExtensions(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = strings.ReplaceAll(raw, ",", " ")

	seen := make(map[string]struct{})
	var out []string
	for _, tok := range strings.Fields(raw) {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.HasPrefix(tok, "*.") {
			tok = tok[1:]
		}
		if strings.HasPrefix(tok, "*") {
			// Not a pure extension pattern like "*.ext".
			continue
		}
		if strings.ContainsAny(tok, "*?[]") {
			continue
		}
		if !strings.HasPrefix(tok, ".") {
			tok = "." + tok
		}
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "." {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out
}

