package registry

// persist.go —— **实例创建参数的持久化**（软重启与进程重启共用）。
//
// ## 用户裁定（2026-11-18 复核 todo）
//
// > 「你好好看看 todo 呢？驻留子的持久化部分你没做」
//
// 原实现的问题：`PersistResidents` 只在**驻留子**被创建/销毁时写登记表，
// 而 `RestoreResidents` **零调用方** —— 重启后所有驻留子消失。
//
// 现在把持久化收在**实例管理器**这一层（不按类型各写一份）：
//
//	创建 → remember（内存）+ Save（落盘）
//	销毁 → forget + Save
//	进程启动 → Load + 按 type 重新 Construct
//
// 于是"新增一种可持久化的子 agent 类型"不需要再写一遍持久化逻辑 ——
// 只要该类型的 Construct 能接受同样的参数（幂等）。
//
// ## 为什么不存运行态
//
// 运行态（轮次、处理表、contextfull 标记）属于实例自己的 context.json / graph.db。
// 这里只存**创建参数**：那是父的决定，不能重算。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// instanceFile 是实例登记表文件名（相对 <data>/agents/）。
const instanceFile = "instances.json"

// InstanceFile 是落盘文件名（导出，供判据与状态面引用）。
func FileName() string { return instanceFile }

// persistedInstance 是落盘的一条。
type persistedInstance struct {
	Type    string                 `json:"type"`
	ID      string                 `json:"id"`
	Arg     map[string]interface{} `json:"arg"`
	Created string                 `json:"created"`
}

// persistedFile 是落盘的整体结构（带版本号，便于将来迁移）。
type persistedFile struct {
	Version   int                 `json:"version"`
	Instances []persistedInstance `json:"instances"`
}

// persistVersion 是当前落盘格式版本。
const persistVersion = 1

// Save 把创建参数落到 <dataDir>/agents/instances.json。
//
// 原子写（先写临时文件再 rename）：进程在写一半时崩溃不能把登记表写坏，
// 那会让下次启动读不出来、全部实例丢失。
func Save(dataDir string, recs []Record) error {
	if strings.TrimSpace(dataDir) == "" {
		return nil // 未配置工作目录 ⇒ 不持久化（单测/独立工具）
	}
	dir := filepath.Join(dataDir, "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("registry: 创建 %s 失败: %w", dir, err)
	}
	out := persistedFile{Version: persistVersion}
	for _, r := range recs {
		if r.ID == "" || r.Type == "" {
			continue // 残缺记录不落盘（宁可少存一条，不可写坏读侧）
		}
		out.Instances = append(out.Instances, persistedInstance{
			Type: r.Type, ID: r.ID, Arg: r.Arg, Created: r.Created,
		})
	}
	sort.Slice(out.Instances, func(i, j int) bool { return out.Instances[i].ID < out.Instances[j].ID })

	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: 序列化实例登记表失败: %w", err)
	}
	tmp := filepath.Join(dir, instanceFile+".tmp")
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("registry: 写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, instanceFile)); err != nil {
		return fmt.Errorf("registry: 提交实例登记表失败: %w", err)
	}
	return nil
}

// Load 读回创建参数快照（文件不存在 ⇒ 空，不报错）。
func Load(dataDir string) ([]Record, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, nil
	}
	path := filepath.Join(dataDir, "agents", instanceFile)
	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("registry: 读实例登记表失败: %w", err)
	}
	var f persistedFile
	if err := json.Unmarshal(buf, &f); err != nil {
		// ★ 读侧坏掉**不静默**：返回错误让调用方决定隔离还是重建。
		//   静默当成空表会让"重启后全部实例消失"看起来像正常行为。
		return nil, fmt.Errorf("registry: 实例登记表损坏（%s）: %w", path, err)
	}
	out := make([]Record, 0, len(f.Instances))
	for _, x := range f.Instances {
		out = append(out, Record{Type: x.Type, ID: x.ID, Arg: x.Arg, Created: x.Created})
	}
	return out, nil
}

// Record 是一条创建参数快照（实例管理器与持久化共用）。
type Record struct {
	Type    string
	ID      string
	Arg     map[string]interface{}
	Created string
}

// Restore 按快照重建实例（进程启动时调一次）。
//
// 返回值：成功数、失败详情（失败**不阻断**其余实例的恢复 ——
// 一个类型被撤销/参数不兼容不该让全部子 agent 起不来）。
func Restore(host Host, recs []Record) (int, []string) {
	if host == nil || len(recs) == 0 {
		return 0, nil
	}
	var ok int
	var errs []string
	for _, r := range recs {
		if r.Type == "" || r.ID == "" {
			continue
		}
		d, found := Lookup(r.Type)
		if !found {
			errs = append(errs, fmt.Sprintf("%s: 类型 %q 未注册（跳过）", r.ID, r.Type))
			continue
		}
		if _, exists := host.GetChild(r.ID); exists {
			continue // 已存在（热重载路径）
		}
		arg := r.Arg
		if arg == nil {
			arg = map[string]interface{}{}
		}
		arg["id"] = r.ID
		if _, err := d.Construct(host, arg); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.ID, err))
			continue
		}
		ok++
	}
	return ok, errs
}
