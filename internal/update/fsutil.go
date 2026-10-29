package update

// 文件写入的**唯一**原语。
//
// ## 为什么必须收敛到这里
//
// 全仓曾经有 5 处各自实现的「写临时文件 → rename」：
//
//	internal/memory/pipeline/pipeline.go  writeFileAtomic
//	internal/security/store.go            写密钥库
//	internal/update/release.go            downloadFile
//	internal/update/kernel.go             CopyFile
//	internal/update/rollback.go           （PR 前是直接 os.WriteFile）
//
// 它们的行为不一致，而且**只有一处**在 rename 前 fsync。不一致的方向
// 恰恰是危险的那一侧：没有 fsync 的 rename 在掉电后可能留下**零长度**
// 或**半截**的目标文件 —— rename 的原子性只保证"名字指向谁"，不保证
// "内容已落盘"。
//
// 升级路径上这个问题不可接受：homed 被替换成半截文件 ⇒ 内核起不来，
// 而 pending 标记可能已经清了 ⇒ 无法回滚。
//
// ## 三条不变量
//
//  1. 目标路径在任何时刻要么是**完整的旧内容**、要么是**完整的新内容**
//  2. 返回 nil 意味着内容**已落盘**（fsync 过），不只是交给了页缓存
//  3. 失败时**不动**目标文件（临时文件被清掉）

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// 临时文件后缀。刻意区分于 Apply 的 ".tmp-upgrade"：
// 那个是旧实现的遗留命名，新代码统一用 WriteFileAtomic/CopyFileAtomic。
const atomicTmpSuffix = ".tmp-atomic"

// WriteFileAtomic 原子地写入文件（临时文件 → fsync → rename → fsync 目录）。
//
// perm 只在新文件创建时生效；目标已存在时保留其原有权限位
// （升级替换 homed 时不能因为传了 0644 就把可执行位丢掉）。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("建目录 %s: %w", dir, err)
	}
	mode := perm
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + atomicTmpSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("建临时文件 %s: %w", tmp, err)
	}
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("写 %s: %w", tmp, err)
	}
	// ★ fsync 必须在 Close 之前：Close 只把数据交给内核页缓存。
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, path, err)
	}
	syncDir(dir)
	return nil
}

// CopyFileAtomic 原子地复制文件（用于替换可执行文件）。
//
// 与 WriteFileAtomic 的差别只有来源：这里流式拷贝，避免把 88MB 的
// 二进制整个读进内存。
//
// ★ 权限位取**源文件**的（升级包里的 homed 是 0755，必须保留可执行位）。
func CopyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("建目录 %s: %w", dir, err)
	}
	tmp := dst + atomicTmpSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return fmt.Errorf("建临时文件 %s: %w", tmp, err)
	}
	cleanup := func() {
		out.Close()
		os.Remove(tmp)
	}
	if _, err := io.Copy(out, in); err != nil {
		cleanup()
		return fmt.Errorf("拷贝 %s → %s: %w", src, tmp, err)
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, dst, err)
	}
	syncDir(dir)
	return nil
}

// syncDir 让 rename 本身落盘。
//
// ★ 失败只忽略，不报错：部分文件系统（overlay/tmpfs/容器层）对目录
// fsync 返回 EINVAL。此时 rename 已完成、内容已正确，为此让升级失败
// 只会把可用环境弄成不可用。方向是安全的：最坏情况是掉电后连 rename
// 都没生效，磁盘上留的仍是**完整的旧文件**。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
