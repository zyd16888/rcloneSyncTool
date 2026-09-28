package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"115togd/internal/store"
)

// Publication calls an actual directory metadata operation. rclone moveto is
// deliberately not used here because it may fall back to moving individual files.
func publishDirectory(ctx context.Context, rule store.Rule, settings store.RuntimeSettings, stagePath string) error {
	config, err := remoteConfig(ctx, settings, rule.DstRemote)
	if err != nil {
		return err
	}
	switch config["type"] {
	case "local":
		return publishLocalDirectory(ctx, rule, settings, stagePath)
	case "drive":
		return publishDriveDirectory(ctx, rule, settings, stagePath, config)
	default:
		return fmt.Errorf("目标后端 %s 尚不支持整目录发布，请使用支持的 local/drive 后端或按目录分组传输", config["type"])
	}
}

func remoteConfig(ctx context.Context, settings store.RuntimeSettings, name string) (map[string]string, error) {
	out, err := metadataCommand(ctx, settings, "config", "dump")
	if err != nil {
		return nil, errors.New("无法读取目标后端类型")
	}
	var configs map[string]map[string]string
	if err := json.Unmarshal(out, &configs); err != nil {
		return nil, errors.New("无法解析目标后端配置")
	}
	config, ok := configs[name]
	if !ok {
		return nil, errors.New("目标 remote 不存在")
	}
	applyDriveOptions(ctx, config)
	return config, nil
}
func publishLocalDirectory(ctx context.Context, rule store.Rule, settings store.RuntimeSettings, stagePath string) error {
	stageRoot, err := localRemoteRoot(ctx, rule.DstRemote, stagePath, settings)
	if err != nil {
		return err
	}
	parent, err := localRemoteRoot(ctx, rule.DstRemote, path.Dir(rule.DstPath), settings)
	if err != nil {
		return err
	}
	target := filepath.Join(parent, path.Base(rule.DstPath))
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return errors.New("正式目录已存在或无法确认其状态")
	}
	if err := os.Rename(stageRoot, target); err != nil {
		return fmt.Errorf("目标文件系统不支持该目录的原子改名：%w", err)
	}
	return nil
}
func localRemoteRoot(ctx context.Context, remote, root string, settings store.RuntimeSettings) (string, error) {
	out, err := metadataCommand(ctx, settings, "backend", "features", remote+":"+root)
	if err != nil {
		return "", err
	}
	var features struct {
		Root     string
		Features struct{ DirMove bool }
	}
	if err := json.Unmarshal(out, &features); err != nil {
		return "", err
	}
	if !features.Features.DirMove || features.Root == "" {
		return "", errors.New("目标文件系统不支持目录改名")
	}
	return filepath.Abs(features.Root)
}

var driveAPIBase = "https://www.googleapis.com/drive/v3"

func publishDriveDirectory(ctx context.Context, rule store.Rule, settings store.RuntimeSettings, stagePath string, config map[string]string) error {
	statDir := func(p string) (lsjsonEntry, error) {
		out, err := metadataCommand(ctx, settings, "lsjson", rule.DstRemote+":"+p, "--stat")
		if err != nil {
			return lsjsonEntry{}, err
		}
		var e lsjsonEntry
		err = json.Unmarshal(out, &e)
		if err == nil && (!e.IsDir || e.ID == "") {
			err = errors.New("目标后端未返回可验证的目录 ID")
		}
		return e, err
	}
	stage, err := statDir(stagePath)
	if err != nil {
		return err
	}
	oldParent, err := statDir(path.Dir(stagePath))
	if err != nil {
		return err
	}
	newParent, err := statDir(path.Dir(rule.DstPath))
	if err != nil {
		return err
	}
	// The metadata reads above let rclone refresh its token. Read the current
	// token after those operations; it never enters logs, responses or job snapshots.
	config, err = remoteConfig(ctx, settings, rule.DstRemote)
	if err != nil {
		return err
	}
	token, err := driveAccessToken(ctx, config)
	if err != nil {
		return err
	}
	query := url.Values{"addParents": {newParent.ID}, "removeParents": {oldParent.ID}, "supportsAllDrives": {"true"}, "fields": {"id,name,parents"}}
	body, _ := json.Marshal(map[string]string{"name": path.Base(rule.DstPath)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, driveAPIBase+"/files/"+url.PathEscape(stage.ID)+"?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Drive 目录发布请求失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Drive 目录发布失败（HTTP %d）", resp.StatusCode)
	}
	var published struct {
		ID, Name string
		Parents  []string
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&published); err != nil {
		return err
	}
	if published.ID != stage.ID || published.Name != path.Base(rule.DstPath) {
		return errors.New("Drive 目录发布结果不匹配")
	}
	for _, id := range published.Parents {
		if id == newParent.ID {
			return nil
		}
	}
	return errors.New("Drive 目录发布未完成父目录更新")
}

func ensureDestinationParent(ctx context.Context, rule store.Rule, settings store.RuntimeSettings) error {
	_, err := metadataCommand(ctx, settings, "mkdir", rule.DstRemote+":"+path.Dir(rule.DstPath))
	return err
}

func validateStage(rule store.Rule, stage string) error {
	if !strings.HasPrefix(stage, strings.TrimSuffix(rule.StagingPath, "/")+"/") || path.Clean(stage) != stage {
		return errors.New("暂存目录越出规则暂存范围")
	}
	return nil
}
