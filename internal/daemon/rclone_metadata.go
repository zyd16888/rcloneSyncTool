package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"115togd/internal/store"
)

func sourceSpec(rule store.Rule) string {
	if rule.SrcKind == "local" {
		return rule.SrcLocalRoot
	}
	return rule.SrcRemote + ":" + rule.SrcPath
}
func destinationSpec(rule store.Rule) string { return rule.DstRemote + ":" + rule.DstPath }
func effectiveRule(rule store.Rule, spec TransferSpec) store.Rule {
	if rule.SrcKind == "local" {
		rule.SrcLocalRoot = joinLocalPath(rule.SrcLocalRoot, spec.SourceSubpath)
	} else {
		rule.SrcPath = joinRemotePath(rule.SrcPath, spec.SourceSubpath)
	}
	rule.DstPath = joinRemotePath(rule.DstPath, spec.DestinationSubpath)
	rule.TransferMode = spec.Operation
	return rule
}
func validRelativeFile(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\r\n\x00") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || part == "" || strings.Contains(part, ":") {
			return false
		}
	}
	return true
}
func withinLocalRoot(root, target string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func metadataCommand(ctx context.Context, settings store.RuntimeSettings, args ...string) ([]byte, error) {
	args = append(args, taskOptions(ctx)...)
	ctx, cancel := context.WithTimeout(ctx, settings.ScanTimeout)
	defer cancel()
	if settings.RcloneConfigPath != "" {
		args = append(args, "--config", settings.RcloneConfigPath)
	}
	out, err := exec.CommandContext(ctx, "rclone", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("rclone %s: %s", args[0], redactMessage(strings.TrimSpace(string(out))))
	}
	return out, nil
}
func statSource(ctx context.Context, rule store.Rule, settings store.RuntimeSettings) (lsjsonEntry, error) {
	if rule.SrcKind == "local" {
		info, err := os.Stat(rule.SrcLocalRoot)
		if err != nil {
			return lsjsonEntry{}, err
		}
		return lsjsonEntry{Path: info.Name(), IsDir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339Nano)}, nil
	}
	out, err := metadataCommand(ctx, settings, "lsjson", sourceSpec(rule), "--stat")
	if err != nil {
		return lsjsonEntry{}, err
	}
	var info lsjsonEntry
	err = json.Unmarshal(out, &info)
	return info, err
}
func listDestination(ctx context.Context, rule store.Rule, settings store.RuntimeSettings) (map[string]lsjsonEntry, error) {
	out, err := metadataCommand(ctx, settings, "lsjson", destinationSpec(rule), "--recursive", "--files-only")
	if err != nil {
		return nil, err
	}
	var entries []lsjsonEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, err
	}
	result := map[string]lsjsonEntry{}
	for _, e := range entries {
		if !e.IsDir {
			result[e.Path] = e
		}
	}
	return result, nil
}
func destinationExists(ctx context.Context, rule store.Rule, settings store.RuntimeSettings) (bool, error) {
	if rule.DstPath == "/" {
		return true, nil
	}
	// Listing the parent distinguishes a missing directory from a network or
	// permission error. A failed stat alone must never authorize publishing.
	parent := rule
	parent.DstPath = path.Dir(rule.DstPath)
	out, err := metadataCommand(ctx, settings, "lsjson", destinationSpec(parent), "--dirs-only", "--max-depth", "1")
	if err != nil {
		return false, err
	}
	var entries []lsjsonEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return false, err
	}
	for _, e := range entries {
		if strings.TrimSuffix(e.Path, "/") == path.Base(rule.DstPath) {
			return true, nil
		}
	}
	return false, nil
}
func verifiedPaths(files []store.TransferJobFile, entries map[string]lsjsonEntry) []string {
	var done []string
	for _, f := range files {
		if e, ok := entries[f.Path]; ok && e.Size == f.Size {
			done = append(done, f.Path)
		}
	}
	return done
}
func manifestMatches(files []store.TransferJobFile, entries map[string]lsjsonEntry) bool {
	return len(verifiedPaths(files, entries)) == len(files)
}
func sourceUnchanged(ctx context.Context, rule store.Rule, settings store.RuntimeSettings, files []store.TransferJobFile, allowMissing bool) ([]string, error) {
	if allowMissing && rule.SrcKind == "local" {
		if _, err := os.Stat(rule.SrcLocalRoot); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
	}
	entries, err := scanRule(ctx, rule, settings)
	if err != nil {
		if allowMissing && rule.SrcKind == "local" && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	index := map[string]store.ScanEntry{}
	for _, e := range entries {
		index[e.Path] = e
	}
	var present []string
	for _, f := range files {
		e, ok := index[f.Path]
		if !ok {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("源文件在任务执行期间消失：%s", f.Path)
		}
		if e.Size != f.Size || (f.ModTime != "" && !e.ModTime.IsZero() && e.ModTime.UTC().Format(time.RFC3339Nano) != f.ModTime) {
			return nil, fmt.Errorf("源文件在任务执行期间变化：%s", f.Path)
		}
		if rule.SrcKind == "local" && !withinLocalRoot(rule.SrcLocalRoot, filepath.Join(rule.SrcLocalRoot, filepath.FromSlash(f.Path))) {
			return nil, fmt.Errorf("源文件越出规则目录：%s", f.Path)
		}
		present = append(present, f.Path)
	}
	return present, nil
}
