package daemon

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"time"

	"115togd/internal/store"
)

func prepareTransfer(ctx context.Context, rule store.Rule, spec TransferSpec, settings store.RuntimeSettings) (TransferSpec, error) {
	effective := effectiveRule(rule, spec)
	if rule.SrcKind == "local" && !withinLocalRoot(rule.SrcLocalRoot, effective.SrcLocalRoot) {
		return spec, errors.New("源目录无法验证或越出规则根目录")
	}
	info, err := statSource(ctx, effective, settings)
	if err != nil {
		return spec, err
	}
	if !info.IsDir {
		if rule.AtomicPublish {
			return spec, errors.New("整目录发布不能以单个文件作为源")
		}
		if rule.SrcKind == "local" {
			rule.SrcLocalRoot = filepath.Dir(effective.SrcLocalRoot)
		} else {
			rule.SrcPath = path.Dir(effective.SrcPath)
		}
		spec.SourceSubpath = ""
		base := path.Base(info.Path)
		if len(spec.Files) > 0 && (len(spec.Files) != 1 || spec.Files[0].Path != base) {
			return spec, errors.New("单文件源的清单必须使用原始文件名")
		}
		spec.Files = []store.TransferJobFile{{Path: base, Size: info.Size, ModTime: info.ModTime, SourcePath: base}}
	} else {
		entries, err := scanRule(ctx, effective, settings)
		if err != nil {
			return spec, err
		}
		entries, err = store.TransferEntries(rule, entries)
		if err != nil {
			return spec, err
		}
		catalog := map[string]store.ScanEntry{}
		for _, e := range entries {
			catalog[e.Path] = e
		}
		requested := spec.Files
		spec.Files = nil
		if len(requested) == 0 {
			for _, e := range entries {
				requested = append(requested, store.TransferJobFile{Path: e.Path})
			}
		}
		for _, f := range requested {
			e, ok := catalog[f.Path]
			if !ok {
				return spec, fmt.Errorf("源文件不存在或被规则过滤：%s", f.Path)
			}
			f.Size = e.Size
			if !e.ModTime.IsZero() {
				f.ModTime = e.ModTime.UTC().Format(time.RFC3339Nano)
			}
			f.SourcePath = path.Join(spec.SourceSubpath, f.Path)
			f.GroupKey, _ = store.FileGroupKey(rule, f.SourcePath)
			spec.Files = append(spec.Files, f)
		}
		if rule.AtomicPublish && len(spec.Files) != len(entries) {
			return spec, errors.New("整目录发布必须包含完整目录清单")
		}
	}
	if len(spec.Files) == 0 {
		return spec, errors.New("没有符合规则的文件，检查最小文件大小、扩展名及过滤参数")
	}
	spec.RuleSnapshot = &rule
	spec.Prepared = true
	return spec, nil
}

// Reuse only staging files whose size and source modification time still match.
// The explicit remaining list keeps quota reservation and the copy in agreement.
func stagedFilesToTransfer(ctx context.Context, rule store.Rule, settings store.RuntimeSettings, stagePath string, files []store.TransferJobFile) ([]store.TransferJobFile, error) {
	if stagePath == "" {
		return files, nil
	}
	if err := validateStage(rule, stagePath); err != nil {
		return nil, err
	}
	stageRule := rule
	stageRule.DstPath = stagePath
	if err := ensureDestinationParent(ctx, stageRule, settings); err != nil {
		return nil, err
	}
	exists, err := destinationExists(ctx, stageRule, settings)
	if err != nil || !exists {
		return files, err
	}
	entries, err := listDestination(ctx, stageRule, settings)
	if err != nil {
		return nil, err
	}
	var remaining []store.TransferJobFile
	for _, f := range files {
		e, ok := entries[f.Path]
		sourceTime, sourceErr := time.Parse(time.RFC3339Nano, f.ModTime)
		stageTime, stageErr := time.Parse(time.RFC3339Nano, e.ModTime)
		if !ok || e.Size != f.Size || sourceErr != nil || stageErr != nil || !stageTime.Equal(sourceTime) {
			remaining = append(remaining, f)
		}
	}
	return remaining, nil
}

func taskReservationBytes(ctx context.Context, job store.TransferJob, spec TransferSpec, settings store.RuntimeSettings) (int64, error) {
	files := spec.Files
	rule := effectiveRule(*spec.RuleSnapshot, spec)
	if rule.AtomicPublish {
		if job.Phase == "cleanup" {
			return 0, nil
		}
		if job.Phase == "publishing" {
			exists, err := destinationExists(ctx, rule, settings)
			if err != nil {
				return 0, err
			}
			if exists {
				return 0, nil
			}
		}
		var err error
		files, err = stagedFilesToTransfer(ctx, rule, settings, job.StagePath, files)
		if err != nil {
			return 0, err
		}
	}
	var bytes int64
	for _, f := range files {
		bytes += f.Size
	}
	return bytes, nil
}
