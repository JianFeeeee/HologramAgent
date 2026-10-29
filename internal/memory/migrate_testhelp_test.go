package memory

// 测试脚手架：重建被测试删掉的 memory_blocks 表。
// 刻意放在 _test.go 里 —— 它不是生产符号，只是让回滚测试能自造失败。
//
// ★ 2026-10-29：这里的 `ddlMemoryBlocks` 副本已**删除**，改用生产同名常量。
//
// 原副本的注释自己就写着「它是重复的真相源 —— 加列时必须两边都改」，
// 并记录了两次因不同步而失败的现场：
//   · 2026-10-04 加 semantic_type → "no such column: semantic_type"
//   · 2026-10-29 加段页式 6 列    → "no such column: frame_ts"
//
// 一份「靠人记得同步」的副本，在第一次忘记时就已经证明了它不是好设计。
// 现在 DDL 只有一处定义（graph.go 的 ddlMemoryBlocks），生产与测试共用。
