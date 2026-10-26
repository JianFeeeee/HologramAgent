package tracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

type Tracker struct {
	mu              sync.Mutex
	dataDir         string
	workDir         string
	lowerDir        string
	upperDir        string
	mergeDir        string
	mounted         bool
	active          bool
	before          *FSState
	changeSets      []*ChangeSet
	keepChangesets  int           // 保留最近 N 份 changeset，0 = 不限
	maxChangesetAge time.Duration // changeset 最大保留时长，0 = 不限
}

type TrackerOption func(*Tracker)

func WithKeepChangesets(n int) TrackerOption {
	return func(t *Tracker) { t.keepChangesets = n }
}

func WithMaxChangesetAge(d time.Duration) TrackerOption {
	return func(t *Tracker) { t.maxChangesetAge = d }
}

func NewTracker(dataDir, workDir string, opts ...TrackerOption) *Tracker {
	t := &Tracker{
		dataDir:    dataDir,
		workDir:    workDir,
		lowerDir:   filepath.Join(workDir, "lower"),
		upperDir:   filepath.Join(workDir, "upper"),
		mergeDir:   filepath.Join(workDir, "merged"),
		changeSets: make([]*ChangeSet, 0),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *Tracker) Init() error {
	for _, d := range []string{t.lowerDir, t.upperDir, t.mergeDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("create overlay dir %s: %w", d, err)
		}
	}
	t.cleanupChangeSets()
	log.Printf("[tracker] initialized (work=%s)", t.workDir)
	return nil
}

func (t *Tracker) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mounted {
		return nil
	}

	if err := t.mountOverlay(); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	t.mounted = true
	t.active = true

	t.before = t.capture()

	log.Printf("[tracker] overlay mounted at %s", t.mergeDir)
	return nil
}

func (t *Tracker) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.mounted {
		return nil
	}

	if err := t.umountOverlay(); err != nil {
		return fmt.Errorf("umount overlay: %w", err)
	}
	t.mounted = false
	t.active = false
	return nil
}

func (t *Tracker) PreAction(action string) *ChangeSet {
	t.mu.Lock()
	defer t.mu.Unlock()

	cs := NewChangeSet(action)
	t.before = t.capture()
	return cs
}

func (t *Tracker) PostAction(action string) *ChangeSet {
	t.mu.Lock()
	defer t.mu.Unlock()

	after, _ := captureFSState(t.upperDir)
	changes := diffStates(t.before, after)

	cs := NewChangeSet(action)
	cs.Files = changes
	if len(changes) > 0 {
		t.changeSets = append(t.changeSets, cs)
		t.saveChangeSet(cs)
		log.Printf("[tracker] action=%s changed=%d files", action, len(changes))
		for _, f := range changes {
			log.Printf("  %s: %s", f.Type, f.Path)
		}
	}

	t.before = t.capture()
	return cs
}

func (t *Tracker) HasChanges() bool {
	return len(t.changeSets) > 0
}

func (t *Tracker) ChangeSets() []*ChangeSet {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]*ChangeSet, len(t.changeSets))
	copy(result, t.changeSets)
	return result
}

func (t *Tracker) capture() *FSState {
	state, err := captureFSStateWithContent(t.upperDir)
	if err != nil {
		return &FSState{Files: make(map[string]FileChange), Root: t.upperDir}
	}
	return state
}

// ErrOverlayNoPrivilege 表示**权限不足**导致 overlay 挂不上（不是不支持）。
//
// 用途：上层据此给出可执行说明（降权运行 vs 提权运行），而不是一律说
// "no overlayfs support?" —— 那会让排查方向完全错。
var ErrOverlayNoPrivilege = errors.New("overlayfs requires mount privilege")

func (t *Tracker) mountOverlay() error {
	workDir := filepath.Join(t.workDir, "work")
	os.MkdirAll(workDir, 0755)

	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", t.lowerDir, t.upperDir, workDir)

	// ★ 直接走 mount(2)，**不要** exec `/bin/mount`（2026-10-26）。
	//
	// 原因：util-linux 的 mount 在 euid != 0 时会**提前**拒绝
	// （"must be superuser to use mount"），即使进程持有 CAP_SYS_ADMIN
	// 环境能力。而内核的判定是 ns_capable(CAP_SYS_ADMIN) —— 环境能力足够。
	//
	// 现场：服务降权到 hologramagent 后，overlay 一直挂不上（exit status 32），
	// 而 systemd 已给 AmbientCapabilities=CAP_SYS_ADMIN。
	// 换成 syscall.Mount 后同一能力集即可生效。
	if err := unix.Mount("overlay", t.mergeDir, "overlay", 0, data); err != nil {
		// ★ 区分"没权限"与"不支持"（2026-10-26 迁移后实测）：
		// 服务降权到非 root 时，即使持有 CAP_SYS_ADMIN，overlay mount 仍可能
		// 返回 EPERM。此时上层若统一报 "no overlayfs support?" 会误导排查
		// （看起来像内核不支持，实际是权限模型选择）。
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("%w (lower=%s upper=%s): %v", ErrOverlayNoPrivilege, t.lowerDir, t.upperDir, err)
		}
		return fmt.Errorf("mount overlayfs failed (lower=%s upper=%s): %w", t.lowerDir, t.upperDir, err)
	}
	return nil
}

func (t *Tracker) umountOverlay() error {
	if err := unix.Unmount(t.mergeDir, 0); err != nil {
		// EBUSY/MNT_EXPIRE 等：换懒卸载再试一次（与 `umount -l` 等价）。
		if err2 := unix.Unmount(t.mergeDir, unix.MNT_DETACH); err2 != nil {
			return fmt.Errorf("umount overlayfs failed: %w (lazy retry: %v)", err, err2)
		}
	}
	return nil
}

func (t *Tracker) saveChangeSet(cs *ChangeSet) {
	dir := filepath.Join(t.dataDir, "changesets")
	os.MkdirAll(dir, 0755)

	path := filepath.Join(dir, cs.ID+".json")
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		log.Printf("[tracker] save changeset %s: %v", cs.ID, err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("[tracker] write changeset %s: %v", cs.ID, err)
	}

	// Content 字段是 json:"-"，不随 JSON 落盘；为保证 L2 离线回滚（guard 重启后）
	// 仍能还原被改/被删文件，把回滚原文作为伴随 blob 单独持久化。
	t.saveContentBlobs(cs, dir)
}

// saveContentBlobs 把 changeset 中各文件的回滚原文写入 <dataDir>/changesets/<csID>_blobs/<i>.bin。
func (t *Tracker) saveContentBlobs(cs *ChangeSet, dir string) {
	blobDir := filepath.Join(dir, cs.ID+"_blobs")
	os.MkdirAll(blobDir, 0755)
	for i, f := range cs.Files {
		if len(f.Content) == 0 {
			continue
		}
		bp := filepath.Join(blobDir, fmt.Sprintf("%03d.bin", i))
		if err := os.WriteFile(bp, f.Content, 0600); err != nil {
			log.Printf("[tracker] write blob %s: %v", bp, err)
		}
	}
}

// loadContentBlobs 读回 <csID>_blobs 目录中的回滚原文到 FileChange.Content。
func (t *Tracker) loadContentBlobs(cs *ChangeSet, dir string) {
	blobDir := filepath.Join(dir, cs.ID+"_blobs")
	for i := range cs.Files {
		bp := filepath.Join(blobDir, fmt.Sprintf("%03d.bin", i))
		if data, err := os.ReadFile(bp); err == nil {
			cs.Files[i].Content = data
		}
	}
}

// removeContentBlobs 删除一个 changeset 的 blob 目录。
func removeContentBlobs(dir, id string) {
	os.RemoveAll(filepath.Join(dir, id+"_blobs"))
}

func (t *Tracker) cleanupChangeSets() {
	dir := filepath.Join(t.dataDir, "changesets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type csFile struct {
		name string
		info os.FileInfo
	}
	var files []csFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, csFile{name: e.Name(), info: info})
	}

	// 按修改时间排序
	sort.Slice(files, func(i, j int) bool {
		return files[i].info.ModTime().Before(files[j].info.ModTime())
	})

	now := time.Now()
	remaining := make([]csFile, 0, len(files))

	for _, f := range files {
		keep := true

		if t.maxChangesetAge > 0 && now.Sub(f.info.ModTime()) > t.maxChangesetAge {
			keep = false
		}

		if keep {
			remaining = append(remaining, f)
		}
	}

	// 再按数量裁剪
	if t.keepChangesets > 0 && len(remaining) > t.keepChangesets {
		excess := len(remaining) - t.keepChangesets
		for i := 0; i < excess; i++ {
			path := filepath.Join(dir, remaining[i].name)
			os.Remove(path)
			removeContentBlobs(dir, strings.TrimSuffix(remaining[i].name, ".json"))
		}
		remaining = remaining[excess:]
	}

	if len(files) != len(remaining) {
		log.Printf("[tracker] cleanup: removed %d changesets, kept %d",
			len(files)-len(remaining), len(remaining))
	}
}

// Rollback 全量回滚：按时间逆序应用每个 changeset 的逆操作，把工作区恢复到
// 首条 changeset 之前的状态（用捕获的原文还原被改/被删文件、删除新增文件）。
// 无任何 changeset 时退回整目录重置（移除 upper 重建，丢弃全部变更）。
func (t *Tracker) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mounted {
		if err := t.umountOverlay(); err != nil {
			return fmt.Errorf("umount for rollback: %w", err)
		}
	}

	if len(t.changeSets) > 0 {
		for i := len(t.changeSets) - 1; i >= 0; i-- {
			t.applyReverseLocked(t.changeSets[i])
		}
		log.Printf("[tracker] rollback complete: reverted %d change sets", len(t.changeSets))
	} else {
		if err := t.resetUpperLocked(); err != nil {
			return err
		}
		log.Printf("[tracker] rollback complete: no change sets, reset upper dir")
	}

	t.changeSets = nil
	t.before = nil
	t.mounted = false
	return nil
}

// RollbackLatest 仅撤销最近一条 changeset（定向回滚，不动更早的改动）。
func (t *Tracker) RollbackLatest() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.changeSets) == 0 {
		return fmt.Errorf("no change sets to roll back")
	}
	cs := t.changeSets[len(t.changeSets)-1]
	t.applyReverseLocked(cs)
	t.changeSets = t.changeSets[:len(t.changeSets)-1]
	t.before = t.capture()
	log.Printf("[tracker] rolled back latest change set %s (%d files)", cs.ID, len(cs.Files))
	return nil
}

// applyReverseLocked 逆应用一个 changeset：created→删除；modified→写回原文；deleted→用原文重建。
// 调用方须持有写锁。
func (t *Tracker) applyReverseLocked(cs *ChangeSet) {
	for _, f := range cs.Files {
		path := filepath.Join(t.upperDir, filepath.Clean(f.Path))
		switch f.Type {
		case ChangeFileCreated:
			if err := os.RemoveAll(path); err != nil {
				log.Printf("[tracker] rollback remove %s: %v", f.Path, err)
			}
		case ChangeFileModified, ChangeFileDeleted:
			if len(f.Content) == 0 {
				log.Printf("[tracker] rollback %s: original content not captured, skipping", f.Path)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				log.Printf("[tracker] rollback mkdir %s: %v", filepath.Dir(f.Path), err)
				continue
			}
			if err := os.WriteFile(path, f.Content, 0644); err != nil {
				log.Printf("[tracker] rollback restore %s: %v", f.Path, err)
			}
		}
	}
}

// resetUpperLocked 整目录重置（无 changeset 时的兜底），调用方须持有写锁。
func (t *Tracker) resetUpperLocked() error {
	if err := os.RemoveAll(t.upperDir); err != nil {
		return fmt.Errorf("remove upper: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(t.workDir, "work")); err != nil {
		return fmt.Errorf("remove work: %w", err)
	}
	if err := os.MkdirAll(t.upperDir, 0755); err != nil {
		return fmt.Errorf("recreate upper: %w", err)
	}
	return nil
}

func (t *Tracker) MergeDir() string {
	return t.mergeDir
}

func (t *Tracker) Stats() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	totalChanges := 0
	for _, cs := range t.changeSets {
		totalChanges += len(cs.Files)
	}
	return map[string]interface{}{
		"mounted":           t.mounted,
		"active":            t.active,
		"change_sets":       len(t.changeSets),
		"total_changes":     totalChanges,
		"merge_dir":         t.mergeDir,
		"upper_dir":         t.upperDir,
		"keep_changesets":   t.keepChangesets,
		"max_changeset_age": t.maxChangesetAge.String(),
	}
}

// Tracker 直接满足内置 SDK 的 TrackerAPI（复用优先，无需独立适配器）。
var _ sdk.TrackerAPI = (*Tracker)(nil)
