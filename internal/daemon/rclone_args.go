package daemon

import (
	"errors"
	"strings"
	"unicode"
)

// ParseRcloneArgs parses a single command-line string into argv.
// Supports basic quoting with single/double quotes and backslash escapes.
func ParseRcloneArgs(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	var out []string
	var b strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, b.String())
		b.Reset()
	}

	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if !inSingle && !inDouble && unicode.IsSpace(r) {
			flush()
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		return nil, errors.New("参数解析失败：末尾反斜杠未闭合")
	}
	if inSingle || inDouble {
		return nil, errors.New("参数解析失败：引号未闭合")
	}
	flush()
	return out, nil
}

type sanitizedArgs struct {
	Args    []string
	Blocked []string
}

func SanitizeRcloneArgs(args []string) sanitizedArgs {
	var out []string
	var blocked []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		if a == "" {
			continue
		}
		key := a
		if k, _, ok := strings.Cut(a, "="); ok {
			key = k
		}
		keyLower := strings.ToLower(key)

		// Block options that would break our job control / logging / config selection.
		if strings.HasPrefix(keyLower, "--rc") ||
			strings.HasPrefix(keyLower, "--stats") ||
			keyLower == "--log-file" ||
			keyLower == "--files-from" ||
			keyLower == "--files-from-raw" ||
			keyLower == "--files-from-replace" ||
			keyLower == "--config" {
			blocked = append(blocked, a)
			if !strings.Contains(a, "=") && optionNeedsValue(keyLower) && i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return sanitizedArgs{Args: out, Blocked: blocked}
}

// SanitizeRcloneFilterArgs removes filter-related flags that would conflict with --files-from/--files-from-raw.
// This keeps automatic jobs stable even if user provided extra args include filters.
func SanitizeRcloneFilterArgs(args []string) sanitizedArgs {
	var out []string
	var blocked []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		if a == "" {
			continue
		}
		key := a
		hasEq := false
		if k, _, ok := strings.Cut(a, "="); ok {
			key = k
			hasEq = true
		}
		keyLower := strings.ToLower(key)
		if isRcloneFilterKey(keyLower) {
			blocked = append(blocked, a)
			if !hasEq && rcloneFilterNeedsValue(keyLower) && i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return sanitizedArgs{Args: out, Blocked: blocked}
}

func optionNeedsValue(keyLower string) bool {
	switch keyLower {
	case "--log-file", "--files-from", "--files-from-raw", "--files-from-replace", "--config", "--stats":
		return true
	default:
		// Be conservative for rc/stats family when not using = form.
		if strings.HasPrefix(keyLower, "--rc-") || strings.HasPrefix(keyLower, "--stats-") {
			return true
		}
		return false
	}
}

func isRcloneFilterKey(keyLower string) bool {
	if strings.HasPrefix(keyLower, "--exclude") {
		return true
	}
	if strings.HasPrefix(keyLower, "--include") {
		return true
	}
	if strings.HasPrefix(keyLower, "--filter") {
		return true
	}
	switch keyLower {
	case "--min-age", "--max-age", "--min-size", "--max-size", "--ignore-size":
		return true
	case "--delete-excluded":
		return true
	default:
		return false
	}
}

func rcloneFilterNeedsValue(keyLower string) bool {
	switch keyLower {
	case "--exclude", "--include", "--filter",
		"--exclude-from", "--include-from", "--filter-from",
		"--exclude-if-present", "--include-if-present",
		"--min-age", "--max-age", "--min-size", "--max-size":
		return true
	default:
		return false
	}
}
