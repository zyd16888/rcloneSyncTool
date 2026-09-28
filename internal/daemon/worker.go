package daemon

import (
	"115togd/internal/store"
	"context"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ruleWorker struct {
	st              *store.Store
	rule            store.Rule
	pm              *PortManager
	gl              *GlobalLimiter
	jr              *JobRegistry
	scanCh          chan struct{}
	stopCh          chan struct{}
	stopped         atomic.Bool
	cancelMu        sync.Mutex
	cancel          context.CancelFunc
	checkedExisting bool
}

func newRuleWorker(st *store.Store, rule store.Rule, pm *PortManager, gl *GlobalLimiter, jr *JobRegistry) *ruleWorker {
	return &ruleWorker{st: st, rule: rule, pm: pm, gl: gl, jr: jr, scanCh: make(chan struct{}, 1), stopCh: make(chan struct{})}
}
func (w *ruleWorker) setCancel(cancel context.CancelFunc) {
	w.cancelMu.Lock()
	w.cancel = cancel
	stopped := w.stopped.Load()
	w.cancelMu.Unlock()
	if stopped {
		cancel()
	}
}
func (w *ruleWorker) stop() {
	if w.stopped.CompareAndSwap(false, true) {
		close(w.stopCh)
	}
	w.cancelMu.Lock()
	cancel := w.cancel
	w.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (w *ruleWorker) triggerScan() {
	select {
	case w.scanCh <- struct{}{}:
	default:
	}
}
func (w *ruleWorker) run(root context.Context) {
	ctx, cancel := context.WithCancel(root)
	defer cancel()
	w.setCancel(cancel)
	var scans sync.WaitGroup
	scans.Add(1)
	go func() { defer scans.Done(); w.scanLoop(ctx) }()
	defer func() { cancel(); scans.Wait() }()
	if w.rule.SrcKind == "local" && w.rule.LocalWatch {
		scans.Add(1)
		go func() { defer scans.Done(); w.watchLocal(ctx) }()
	}
	settings, err := w.st.RuntimeSettings(ctx)
	if err != nil {
		log.Printf("rule %s: settings: %v", w.rule.ID, err)
		return
	}
	ticker := time.NewTicker(settings.SchedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.plan(ctx)
			if updated, e := w.st.RuntimeSettings(ctx); e == nil && updated.SchedulerTick != settings.SchedulerTick {
				settings = updated
				ticker.Reset(settings.SchedulerTick)
			}
		}
	}
}
func (w *ruleWorker) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(w.rule.ScanIntervalSec) * time.Second)
	defer ticker.Stop()
	w.triggerScan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.doScan(ctx)
		case <-w.scanCh:
			w.doScan(ctx)
		}
	}
}
func (w *ruleWorker) doScan(ctx context.Context) {
	if err := w.st.StartRuleScan(ctx, w.rule.ID); err != nil {
		return
	}
	settings, err := w.st.RuntimeSettings(ctx)
	var stats store.ScanStats
	if err == nil {
		var entries []store.ScanEntry
		entries, err = scanRule(ctx, w.rule, settings)
		if err == nil {
			stats, err = w.st.ApplyScan(ctx, w.rule, entries)
			if err == nil && !w.checkedExisting {
				err = w.reconcileExisting(ctx, settings)
				if err == nil {
					w.checkedExisting = true
				}
			}
		}
	}
	message := ""
	if err != nil {
		message = redactMessage(err.Error())
		log.Printf("rule %s: scan: %s", w.rule.ID, message)
	}
	final, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := w.st.FinishRuleScan(final, w.rule.ID, stats, message); err != nil {
		log.Printf("rule %s: save scan: %v", w.rule.ID, err)
	}
}
func (w *ruleWorker) plan(ctx context.Context) {
	if w.stopped.Load() || ctx.Err() != nil {
		return
	}
	if _, err := w.st.EnqueueStable(ctx, w.rule.ID, w.rule.BatchSize, 0); err != nil {
		return
	}
	activities, err := w.st.RuleActivities(ctx)
	if err != nil {
		return
	}
	activity := activities[w.rule.ID]
	if activity.Running+activity.Pending >= w.rule.MaxParallelJobs {
		return
	}
	settings, err := w.st.RuntimeSettings(ctx)
	if err != nil {
		return
	}
	groups, err := w.st.QueuedGroups(ctx, w.rule)
	if err != nil {
		return
	}
	if len(groups) == 0 {
		return
	}
	budget, err := w.st.AvailableBudget(ctx, w.rule)
	if err != nil {
		_ = w.st.SetRuleBlock(ctx, w.rule.ID, "configuration", err.Error())
		return
	}
	var selected []store.QueuedGroup
	var files []store.TransferJobFile
	var bytes int64
	layoutBlocked := false
	for _, g := range groups {
		if w.rule.AtomicPublish && g.Directory == "" {
			layoutBlocked = true
			_ = w.st.SetRuleBlock(ctx, w.rule.ID, "source_layout", "整目录发布需要每部影片位于源目录下的独立子目录")
			continue
		}
		if budget >= 0 && g.Bytes > budget-bytes {
			continue
		}
		if len(files) > 0 && (w.rule.AtomicPublish || len(files)+len(g.Files) > w.rule.BatchSize) {
			break
		}
		selected = append(selected, g)
		bytes += g.Bytes
		if w.rule.AtomicPublish {
			files = append(files, store.RelativeGroupFiles(g)...)
		} else {
			files = append(files, g.Files...)
		}
		if len(files) >= w.rule.BatchSize || w.rule.AtomicPublish {
			break
		}
	}
	if len(files) == 0 {
		if layoutBlocked {
			return
		}
		_ = w.st.SetRuleBlock(ctx, w.rule.ID, "quota_exhausted", fmt.Sprintf("完整文件组无法放入剩余配额（剩余 %d 字节）；等待配额恢复或调整上限", budget))
		return
	}
	frozen := w.rule
	spec := TransferSpec{Operation: w.rule.TransferMode, Files: files, RuleSnapshot: &frozen, Prepared: true}
	groupKey := ""
	if w.rule.AtomicPublish {
		spec.SourceSubpath = selected[0].Directory
		spec.DestinationSubpath = selected[0].Directory
		spec.GroupSignature = selected[0].Signature
		groupKey = selected[0].Key
	}
	encoded, err := EncodeTransferSpec(spec)
	if err != nil {
		return
	}
	id := newID()
	job := store.TransferJob{JobID: id, RuleID: w.rule.ID, Origin: store.OriginScheduler, TransferMode: w.rule.TransferMode, RequestJSON: encoded, QuotaGroup: w.rule.LimitGroup, GroupKey: groupKey, LogPath: filepath.Join(settings.LogDir, w.rule.ID, id+".log")}
	if err := w.st.QueueSchedulerTask(ctx, job, selected, files); err != nil {
		log.Printf("rule %s: queue task: %v", w.rule.ID, err)
		return
	}
	_ = w.st.SetRuleBlock(ctx, w.rule.ID, "", "")
}
func (w *ruleWorker) watchLocal(ctx context.Context) {
	root := strings.TrimSpace(w.rule.SrcLocalRoot)
	if root == "" {
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("rule %s: local watch: %v", w.rule.ID, err)
		return
	}
	defer watcher.Close()

	addDir := func(p string) {
		if err := watcher.Add(p); err != nil {
			// ignore
		}
	}

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			addDir(p)
		}
		return nil
	})

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false
	trigger := func() {
		if pending {
			return
		}
		pending = true
		debounce.Reset(600 * time.Millisecond)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-watcher.Errors:
			if err != nil {
				log.Printf("rule %s: local watch error: %v", w.rule.ID, err)
			}
		case ev := <-watcher.Events:
			// Watch new directories recursively.
			if ev.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				fi, err := os.Stat(ev.Name)
				if err == nil && fi.IsDir() {
					_ = filepath.WalkDir(ev.Name, func(p string, d fs.DirEntry, err error) error {
						if err == nil && d.IsDir() {
							addDir(p)
						}
						return nil
					})
				}
			}
			trigger()
		case <-debounce.C:
			pending = false
			w.triggerScan()
		}
	}
}
