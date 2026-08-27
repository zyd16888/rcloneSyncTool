package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type LimitGroup struct {
	Name            string
	DailyLimitBytes int64
	UpdatedAt       time.Time
}

type Remote struct {
	Name       string
	Type       string
	Config     map[string]string
	UpdatedAt  time.Time
	ConfigJSON string
}

type ExtensionPreset struct {
	Name       string
	Extensions string
	UpdatedAt  time.Time
}

func (r *Remote) Normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	r.Type = strings.TrimSpace(r.Type)
	if r.Name == "" {
		return errors.New("remote name required")
	}
	if r.Type == "" {
		return errors.New("remote type required")
	}
	if r.Config == nil {
		r.Config = map[string]string{}
	}
	return nil
}

func (r *Remote) MarshalConfig() error {
	if err := r.Normalize(); err != nil {
		return err
	}
	b, err := json.Marshal(r.Config)
	if err != nil {
		return err
	}
	r.ConfigJSON = string(b)
	return nil
}

func (r *Remote) UnmarshalConfig() error {
	if r.ConfigJSON == "" {
		r.Config = map[string]string{}
		return nil
	}
	return json.Unmarshal([]byte(r.ConfigJSON), &r.Config)
}

type Rule struct {
	ID              string
	LimitGroup      string
	SrcKind         string
	SrcRemote       string
	SrcPath         string
	SrcLocalRoot    string
	LocalWatch      bool
	DstRemote       string
	DstPath         string
	TransferMode    string
	RcloneExtraArgs string
	ResumeEnabled   bool
	PartialDir      string
	PartialSuffix   string
	IgnoreExtensions string
	Bwlimit         string
	DailyLimitBytes int64
	MinFileSizeBytes int64
	IsManual        bool
	APIEnabled      bool
	APIAllowedOperations string
	MaxParallelJobs int
	ScanIntervalSec int
	StableSeconds   int
	BatchSize       int
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (r *Rule) Normalize() error {
	r.ID = strings.TrimSpace(r.ID)
	r.LimitGroup = strings.TrimSpace(r.LimitGroup)
	r.SrcKind = strings.TrimSpace(strings.ToLower(r.SrcKind))
	if r.SrcKind == "" {
		r.SrcKind = "remote"
	}
	if r.SrcKind != "remote" && r.SrcKind != "local" {
		return fmt.Errorf("invalid src_kind: %q", r.SrcKind)
	}
	r.SrcRemote = strings.TrimSpace(r.SrcRemote)
	r.SrcPath = cleanRemotePath(r.SrcPath)
	r.DstRemote = strings.TrimSpace(r.DstRemote)
	r.DstPath = cleanRemotePath(r.DstPath)
	r.SrcLocalRoot = strings.TrimSpace(r.SrcLocalRoot)
	r.TransferMode = strings.TrimSpace(strings.ToLower(r.TransferMode))
	r.RcloneExtraArgs = strings.TrimSpace(r.RcloneExtraArgs)
	r.PartialDir = strings.TrimSpace(r.PartialDir)
	r.PartialSuffix = strings.TrimSpace(r.PartialSuffix)
	r.IgnoreExtensions = strings.TrimSpace(r.IgnoreExtensions)
	if r.TransferMode == "" {
		r.TransferMode = "copy"
	}
	if r.TransferMode != "copy" && r.TransferMode != "move" {
		return fmt.Errorf("invalid transfer_mode: %q", r.TransferMode)
	}
	normalizedOps, err := normalizeOperations(r.APIAllowedOperations)
	if err != nil {
		return err
	}
	r.APIAllowedOperations = normalizedOps
	r.Bwlimit = strings.TrimSpace(r.Bwlimit)
	if r.MinFileSizeBytes < 0 {
		r.MinFileSizeBytes = 0
	}
	if r.DailyLimitBytes < 0 {
		r.DailyLimitBytes = 0
	}
	if r.ID == "" {
		return errors.New("rule id required")
	}
	if r.SrcKind == "remote" {
		if r.SrcRemote == "" {
			return errors.New("src_remote required for src_kind=remote")
		}
		if r.SrcPath == "" {
			return errors.New("src_path required for src_kind=remote")
		}
	}
	if r.SrcKind == "local" {
		if r.SrcLocalRoot == "" {
			return errors.New("src_local_root required for src_kind=local")
		}
		// Allow src_remote/src_path to be empty for local rules.
	}
	if r.DstRemote == "" {
		return errors.New("dst_remote required")
	}
	if r.DstPath == "" {
		return errors.New("dst_path required")
	}
	if r.MaxParallelJobs <= 0 {
		r.MaxParallelJobs = 1
	}
	if r.ScanIntervalSec <= 0 {
		r.ScanIntervalSec = 15
	}
	if r.StableSeconds < 0 {
		r.StableSeconds = 60
	}
	if r.BatchSize <= 0 {
		r.BatchSize = 100
	}
	return nil
}

// AllowedAPIOperations lists the transfer operations /api/v1 may request for
// this rule. An empty allow-list means "only what the rule itself runs", which
// keeps a rule from silently gaining a second behaviour when the API is opened.
func (r *Rule) AllowedAPIOperations() []string {
	ops := splitOperations(r.APIAllowedOperations)
	if len(ops) == 0 {
		mode := strings.TrimSpace(strings.ToLower(r.TransferMode))
		if mode == "" {
			mode = "copy"
		}
		return []string{mode}
	}
	return ops
}

// AllowsAPIOperation reports whether an API caller may request op on this rule.
func (r *Rule) AllowsAPIOperation(op string) bool {
	op = strings.TrimSpace(strings.ToLower(op))
	for _, allowed := range r.AllowedAPIOperations() {
		if allowed == op {
			return true
		}
	}
	return false
}

func splitOperations(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	}) {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func normalizeOperations(raw string) (string, error) {
	seen := map[string]bool{}
	var out []string
	for _, op := range splitOperations(raw) {
		if op != "copy" && op != "move" {
			return "", fmt.Errorf("invalid api operation: %q", op)
		}
		if seen[op] {
			continue
		}
		seen[op] = true
		out = append(out, op)
	}
	return strings.Join(out, ","), nil
}

func cleanRemotePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func ParseEnabled(s string) bool { return parseBool(s) }

func parseIntDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
