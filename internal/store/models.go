package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Remote struct {
	Name       string
	Type       string
	Config     map[string]string
	UpdatedAt  time.Time
	ConfigJSON string
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
	SrcRemote       string
	SrcPath         string
	DstRemote       string
	DstPath         string
	TransferMode    string
	Bwlimit         string
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
	r.SrcRemote = strings.TrimSpace(r.SrcRemote)
	r.SrcPath = cleanRemotePath(r.SrcPath)
	r.DstRemote = strings.TrimSpace(r.DstRemote)
	r.DstPath = cleanRemotePath(r.DstPath)
	r.TransferMode = strings.TrimSpace(strings.ToLower(r.TransferMode))
	if r.TransferMode == "" {
		r.TransferMode = "copy"
	}
	if r.TransferMode != "copy" && r.TransferMode != "move" {
		return fmt.Errorf("invalid transfer_mode: %q", r.TransferMode)
	}
	r.Bwlimit = strings.TrimSpace(r.Bwlimit)
	if r.ID == "" {
		return errors.New("rule id required")
	}
	if r.SrcRemote == "" || r.DstRemote == "" {
		return errors.New("src_remote and dst_remote required")
	}
	if r.SrcPath == "" || r.DstPath == "" {
		return errors.New("src_path and dst_path required")
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
