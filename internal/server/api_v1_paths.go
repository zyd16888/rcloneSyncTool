package server

import (
	"errors"
	"path/filepath"
	"strings"
)

var (
	errPathEscape  = errors.New("path escapes the rule root")
	errPathTooLong = errors.New("path is too long")
)

// normalizeSubpath validates a caller-supplied path fragment and returns it in
// a canonical slash form. Everything an API caller sends is relative to a rule
// root: absolute paths, drive letters, parent traversal and control characters
// are rejected before they can reach an rclone argument.
func normalizeSubpath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) > apiMaxSubpathBytes {
		return "", errPathTooLong
	}
	value = strings.ReplaceAll(value, "\\", "/")
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", errPathEscape
		}
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") {
		return "", errPathEscape
	}
	// A colon in the first segment would look like an rclone remote prefix or a
	// Windows drive letter, either of which leaves the rule root.
	if head, _, _ := strings.Cut(value, "/"); strings.Contains(head, ":") {
		return "", errPathEscape
	}
	var segments []string
	for _, segment := range strings.Split(value, "/") {
		switch segment {
		case "", ".":
			continue
		case "..":
			return "", errPathEscape
		default:
			segments = append(segments, segment)
		}
	}
	if len(segments) == 0 {
		return "", nil
	}
	return strings.Join(segments, "/"), nil
}

// resolveLocalSource confirms that a normalized subpath still resolves inside
// the rule root after symlinks are followed. Path arithmetic alone cannot see a
// symlink pointing out of the root, so the check is done against the real path.
func resolveLocalSource(root, subpath string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errPathEscape
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// A root that does not exist yet cannot be verified; fall back to the
		// lexical form, which still rejects traversal handled above.
		realRoot = filepath.Clean(root)
	}
	target := filepath.Join(realRoot, filepath.FromSlash(subpath))
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		realTarget = filepath.Clean(target)
	}
	if !withinRoot(realRoot, realTarget) {
		return "", errPathEscape
	}
	return target, nil
}

func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
