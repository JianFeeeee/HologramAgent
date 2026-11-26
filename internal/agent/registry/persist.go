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
	// WorkDir 是本实例的工作数据目录（内核派生并落盘）。
	//
	// ★ 为什么落盘而不是恢复时重算：目录里装着这个实例的**全部运行数据**
	// （上下文、图库、临时文件）。重算要求"数据根 + 类型名 + id"三者都不变；
	// 数据根是可配置的（迁移/换盘），落盘后恢复读的是"它当时在哪"，
	// 于是搬迁只需搬目录 + 改表，不需要类型配合。
	WorkDir string `json:"work_dir,omitempty"`
	// Lifecycle 是运行态（running / stopped）。
	//
	// ★ stopped 的实例**不自动拉起**：它是"保留记录但别跑"的显式状态，
	// 重启后无条件拉起会让人无法关掉一个子 agent。
	Lifecycle string `json:"lifecycle,omitempty"`
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
			WorkDir: r.WorkDir, Lifecycle: r.Lifecycle,
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
		out = append(out, Record{
			Type: x.Type, ID: x.ID, Arg: x.Arg, Created: x.Created,
			WorkDir: x.WorkDir, Lifecycle: x.Lifecycle,
		})
	}
	return out, nil
}

// Record 是一条创建参数快照（实例管理器与持久化共用）。
type Record struct {
	Type    string
	ID      string
	Arg     map[string]interface{}
	Created string
	// WorkDir 是本实例的工作数据目录（内核派生）。
	WorkDir string
	// Lifecycle 是运行态（running / stopped；空 = running，存量兼容）。
	Lifecycle string
}

// ShouldRestore 报告本条记录在进程启动时是否应被重建。
//
// ★ 只跳过显式 `stopped`：空值按 running（存量表没有这个字段，
//
//	按"以前能恢复的现在也恢复"处理）。
func (r Record) ShouldRestore() bool {
	return !strings.EqualFold(strings.TrimSpace(r.Lifecycle), "stopped")
}

// ArgWorkDir 是创建参数里"本实例工作目录"的键名。
//
// ★ 它是**内核填的**，不是调用方给的：模型调 admin_child 时不应也不能指定
// 自己的数据目录（那会把"一条记录 = 一个目录"的纪律让给调用方）。
const ArgWorkDir = "work_dir"

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
		if !r.ShouldRestore() {
			continue // 显式 stopped：保留记录，不拉起
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
		// ★ 工作目录由内核传入（类型不拼路径）：
		//   有落盘值就用它（数据在搬迁后仍指向原处），否则现算。
		if dir := strings.TrimSpace(r.WorkDir); dir != "" {
			arg[ArgWorkDir] = dir
		} else if dir := host.WorkDirOf(r.Type, r.ID); dir != "" {
			arg[ArgWorkDir] = dir
		}
		// ★ `Instantiate` 成功 ≠ "这个实例能恢复"。
		//
		// 类型在 `construct` 里可能已经做完一部分（造实例、登记），
		// 而后续步骤（如划入 inputch）失败并返回 error —— 那时实例
		// **不在了**（construct 自己回滚），所以必须计入失败而不是成功。
		//
		// 这里只按 error 判：非 nil 就是没恢复上。`ok++` 只给真正成功的那条。
		if _, _, err := Instantiate(d, host, arg); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.ID, err))
			continue
		}
		// ★ 恢复出来的实例必须**起运行期**（2026-11-25 实测缺陷）。
		//
		// `Instantiate` 只做「construct + Initialize」，**不做 Start**
		// （Start 必须排在面挂好之后，见 registry.StartInstance 的注释）。
		// 恢复路径原先漏了这一句 ⇒ 实例建出来、登记进表、`state=running`，
		// 但 `Runtime.Start` 从未被调 ⇒ **循环没起、任务永远不处理**。
		// 现场：6 个实例恢复成功（`0 skipped`），却没有任何 `[resident:*]` 日志。
		if nh, ok := host.(NewlyCreatedHost); ok {
			StartInstance(nh.NewlyCreated())
		}
		ok++
	}
	return ok, errs
}
