package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// DTO 已迁入内置 SDK，此处保留别名以兼容现有调用方。
type ChangeType = sdk.ChangeType

const (
	ChangeFileCreated  = sdk.ChangeFileCreated
	ChangeFileModified = sdk.ChangeFileModified
	ChangeFileDeleted  = sdk.ChangeFileDeleted
)

type FileChange = sdk.FileChange
type ChangeSet = sdk.ChangeSet

func NewChangeSet(action string) *ChangeSet {
	return &ChangeSet{
		ID:        fmt.Sprintf("cs_%d", time.Now().UnixNano()),
		Action:    action,
		Timestamp: time.Now(),
	}
}

func fileHash(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), int64(len(data)), nil
}

// maxCapturedContent 回滚内容捕获上限：超大文件不保存原文（回滚时跳过并告警）。
const maxCapturedContent = 8 << 20

func fileInfo(path string) (size int64, modTime time.Time, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	return info.Size(), info.ModTime(), nil
}

type FSState struct {
	Files map[string]FileChange `json:"files"`
	Root  string                `json:"root"`
}

// captureFSState 仅记录哈希/尺寸（用于"之后"快照，省内存）。
func captureFSState(root string) (*FSState, error) {
	state := &FSState{
		Files: make(map[string]FileChange),
		Root:  root,
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		hash, size, err := fileHash(path)
		if err != nil {
			return nil
		}
		state.Files[rel] = FileChange{
			Path:      rel,
			HashAfter: hash,
			SizeAfter: size,
		}
		return nil
	})
	return state, err
}

// captureFSStateWithContent 额外捕获文件原文（用于"之前"基线，供回滚还原被改/被删文件）。
func captureFSStateWithContent(root string) (*FSState, error) {
	state, err := captureFSState(root)
	if err != nil {
		return nil, err
	}
	for rel := range state.Files {
		path := filepath.Join(root, rel)
		info, err := os.Stat(path)
		if err != nil || info.Size() > maxCapturedContent {
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			fc := state.Files[rel]
			fc.Content = data
			state.Files[rel] = fc
		}
	}
	return state, nil
}

func diffStates(before, after *FSState) []FileChange {
	var changes []FileChange
	if before == nil || after == nil {
		return changes
	}
	seen := make(map[string]bool)

	for path, afterFile := range after.Files {
		seen[path] = true
		if beforeFile, ok := before.Files[path]; ok {
			if beforeFile.HashAfter != afterFile.HashAfter {
				changes = append(changes, FileChange{
					Path:       path,
					Type:       ChangeFileModified,
					HashBefore: beforeFile.HashAfter,
					HashAfter:  afterFile.HashAfter,
					SizeBefore: beforeFile.SizeAfter,
					SizeAfter:  afterFile.SizeAfter,
					Content:    beforeFile.Content, // 原始内容，供回滚还原
				})
			}
		} else {
			changes = append(changes, FileChange{
				Path:      path,
				Type:      ChangeFileCreated,
				HashAfter: afterFile.HashAfter,
				SizeAfter: afterFile.SizeAfter,
			})
		}
	}

	for path := range before.Files {
		if !seen[path] {
			changes = append(changes, FileChange{
				Path:       path,
				Type:       ChangeFileDeleted,
				HashBefore: before.Files[path].HashAfter,
				SizeBefore: before.Files[path].SizeAfter,
				Content:    before.Files[path].Content, // 原始内容，供回滚还原
			})
		}
	}

	return changes
}
