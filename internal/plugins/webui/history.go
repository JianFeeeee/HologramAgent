package webui

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// defaultHistoryFile 是聊天记录的默认文件名（落在 <data> 下）。
const defaultHistoryFile = "webui_chat_history.json"

// historyStore 把聊天记录存在**独立文件**里。
//
// 背景：原先聊天记录是插件配置项 plugin.webui.chathistory，存在 config.db 的
// config_webui 表里，带来三个后果：
//
//  1. 整段记录（生产实例实测 5,176,016 字节）会被 GET /api/v1/settings
//     当普通配置项整块返回（那次响应实测 8,244,108 字节）；
//  2. 每来一条消息就把整段记录重新 marshal 后 UPDATE 回 config 表，而那次写要拿
//     config registry 的**全局写锁** —— 消息频繁时所有配置读写都被拖着排队；
//  3. 存放位置不可配（想放挂载盘只能改整个 data_dir）。
//
// 现在改为独立文件：默认 <data>/webui_chat_history.json，插件设置 history_file
// 可指定（相对路径按 data 目录解析）。写盘用 tmp+rename 原子替换，
// 避免进程被杀时留下半截 JSON。
type historyStore struct {
	mu   sync.Mutex
	path string
}

func newHistoryStore(path string) *historyStore { return &historyStore{path: path} }

// Path 返回记录文件路径（用于日志与状态展示）。
func (hs *historyStore) Path() string { return hs.path }

// Load 读回全部记录。文件不存在返回 nil；内容损坏时按空历史处理并告警 ——
// 聊天记录不是关键数据，不该因为它让 WebUI 起不来。
func (hs *historyStore) Load() []ChatMsg {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	b, err := os.ReadFile(hs.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[webui] 读取聊天记录 %s 失败: %v", hs.path, err)
		}
		return nil
	}
	var msgs []ChatMsg
	if err := json.Unmarshal(b, &msgs); err != nil {
		log.Printf("[webui] 聊天记录 %s 解析失败（按空历史处理）: %v", hs.path, err)
		return nil
	}
	return msgs
}

// Save 原子写回全部记录（同目录 tmp + rename）。
func (hs *historyStore) Save(msgs []ChatMsg) error {
	b, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if dir := filepath.Dir(hs.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	tmp := hs.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, hs.path)
}

// LoadWithMigration 返回聊天记录：以文件为准，并顺手收拾老版本的配置项。
//
//   - 文件里已有记录：老配置项若还在（上次迁移没删掉），直接清掉；
//   - 文件为空/不存在但老配置项有内容：把记录搬到文件，搬成功后删除配置项
//     —— 这条 5MB 的记录正是 config.db 膨胀与设置接口大响应的来源。
func (hs *historyStore) LoadWithMigration(settings sdk.SettingsAPI) []ChatMsg {
	if msgs := hs.Load(); len(msgs) > 0 {
		if settings != nil {
			if v, err := settings.Get("chathistory"); err == nil && v != nil {
				if err := settings.Remove("chathistory"); err == nil {
					log.Printf("[webui] 已清理历史遗留的配置项 chathistory（记录现存放于 %s）", hs.path)
				}
			}
		}
		return msgs
	}
	return migrateLegacyHistory(settings, hs)
}

// migrateLegacyHistory 把老配置项里的整段聊天记录搬到文件，成功后删掉那条配置。
func migrateLegacyHistory(settings sdk.SettingsAPI, hs *historyStore) []ChatMsg {
	if settings == nil {
		return nil
	}
	v, err := settings.Get("chathistory")
	if err != nil || v == nil {
		return nil
	}
	raw, ok := v.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		// 空值也算遗留（占着设置页一行），直接删掉。
		_ = settings.Remove("chathistory")
		return nil
	}
	var msgs []ChatMsg
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		log.Printf("[webui] 历史配置项无法解析，保留原值不迁移: %v", err)
		return nil
	}
	if err := hs.Save(msgs); err != nil {
		log.Printf("[webui] 聊天记录迁移到 %s 失败（保留原配置项）: %v", hs.path, err)
		return nil
	}
	if err := settings.Remove("chathistory"); err != nil {
		log.Printf("[webui] 聊天记录已迁移到 %s，但旧配置项删除失败: %v", hs.path, err)
	} else {
		log.Printf("[webui] 聊天记录已迁移到独立文件 %s（%d 条），并从插件配置表移除", hs.path, len(msgs))
	}
	return msgs
}

// resolveHistoryFile 解析聊天记录文件路径：
// 插件设置 history_file（非空）> 默认 <data>/webui_chat_history.json。
// 相对路径按 data 目录解析，便于把记录放到独立挂载盘。
func resolveHistoryFile(setting, dataDir string) string {
	if s := strings.TrimSpace(setting); s != "" {
		if filepath.IsAbs(s) {
			return s
		}
		if dataDir == "" {
			dataDir = os.TempDir()
		}
		return filepath.Join(dataDir, s)
	}
	if dataDir == "" {
		// data 目录未知（嵌入/测试场景）：落到系统临时目录，别污染进程当前目录。
		dataDir = os.TempDir()
	}
	return filepath.Join(dataDir, defaultHistoryFile)
}

// settingString 读一个字符串型插件设置（读不到/类型不符都返回空串）。
func settingString(s sdk.SettingsAPI, key string) string {
	if s == nil {
		return ""
	}
	v, err := s.Get(key)
	if err != nil || v == nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return ""
}
