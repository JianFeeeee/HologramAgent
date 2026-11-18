package memory

// artifacts_adapter.go —— 把归一化判定适配成 `internal/artifacts.Norm`。
//
// ## 为什么需要这一层
//
// 基座（`agent/distill.go` 的 `coldDocToTriples`）要判"这个名字能不能落库"
// 与"这个名字是不是噪声"，而它不能 import 本包（带 SQLite）。
// 契约在 `internal/artifacts`（叶子），实现在这里，适配也在这里。
//
// ★ 零值语义（`artifacts.Norm` 的注释）：未装配 ⇒ 放行。
//   所以本函数一定返回**已填满**的 Norm（不能让它退化成零值）。

import "github.com/JianFeeeee/HologramAgent/internal/artifacts"

// ArtifactsNorm 返回本包的归一化判定（装配点注入给 `AgentConfig.Norm`）。
func ArtifactsNorm() artifacts.Norm {
	return artifacts.Norm{
		ValidName:     IsValidEntityName,
		IsNoiseEntity: IsNoiseEntity,
	}
}
