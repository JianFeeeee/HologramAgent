package update

// 差分级别的**发包/取包**：把「整包下载」换成「delta + 本地基线重建」。
//
// ## 为什么需要这一层（实测）
//
// 一次逻辑改动，88MB 的二进制只差 80 字节；而 OTA 下的是整个 tar.gz。
// 差分把这一步从 88MB 降到几十 KB 量级。
//
// ## 为什么 delta 必须绑定旧文件的 sha256
//
// 差分只能对**特定旧版本**应用。用错基线会静默产出一份"看起来正常"
// 的垃圾 —— 大小对、结构对，就是内容错。所以：
//
//	① 构建期在 delta 里写入旧/新两者完整 sha256
//	② 取包时先验本地基线 sha256，不符就**放弃差分，退回整包**
//	③ 应用后验结果 sha256，不符同样退回整包
//
// 回退而不是报错，是这个设计的要点：差分是**优化**，不是可用性前提。

import (
	"fmt"
	"os"
	"path/filepath"
)

// DeltaAssetName 是发布时 delta 资产的命名约定。
//
// 形如：hologramagent-1.4.0-linux-amd64-from-<oldCommit8>.delta
// 带上 from-commit 是为了让下载方一眼看出它对应哪个基线，
// 而不是"随便挑一个 delta 试试"。
func DeltaAssetName(version, goos, goarch, fromCommit string) string {
	return fmt.Sprintf("hologramagent-%s-%s-%s-from-%s.delta", version, goos, goarch, ShortCommit(fromCommit))
}

// TryDeltaUpgrade 尝试用 delta 重建新二进制到 outDir。
//
// 返回值：
//
//	true,  nil  —— 成功重建（outDir/homed 已就位）
//	false, nil  —— 没有可用 delta 或不适用（调用方应退回整包）
//	false, err  —— 出错（同样建议退回整包，但错误应记录）
//
// ## 什么情况下退回整包
//
//	· 本地没有基线二进制（首次安装 / 二进制已被替换）
//	· 发布里没有匹配当前版本的 delta
//	· 基线 sha256 不符（本地版本不是 delta 针对的那个）
//	· 应用后校验失败
//
// 这四种都**不是**致命错误：整包永远可用，差分只是省流量。
func TryDeltaUpgrade(pkgDir, binPath, version string, rel *ReleaseInfo) (bool, error) {
	if rel == nil || rel.DeltaURL == "" {
		return false, nil // 该 release 没提供 delta
	}
	if !FileExists(binPath) {
		return false, nil
	}

	// ① 读本地基线（要用它的字节重建）
	base, err := os.ReadFile(binPath)
	if err != nil {
		return false, fmt.Errorf("读本地基线: %w", err)
	}

	// ② 下载 delta
	patchPath := filepath.Join(pkgDir, "kernel.delta")
	if err := downloadFile(rel.DeltaURL, patchPath); err != nil {
		return false, fmt.Errorf("下载 delta: %w", err)
	}
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		return false, fmt.Errorf("读 delta: %w", err)
	}

	// ③ 应用（内含基线校验 + 结果校验）
	out, err := ApplyDelta(base, patch)
	if err != nil {
		// ★ 退回整包，不当失败：可能是本地版本与 delta 基线不符
		return false, err
	}

	// ④ 落位
	dst := filepath.Join(pkgDir, "bin", "homed")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return false, err
	}
	if err := WriteFileAtomic(dst, out, 0755); err != nil {
		return false, err
	}
	return true, nil
}

// MakeDeltaForRelease 生成发布用的 delta（构建脚本 / 发布流程调用）。
//
// oldBin 是**上一个已发布版本**的二进制，newBin 是本版。
func MakeDeltaForRelease(oldBin, newBin, outPath string) (*DeltaHeader, int64, error) {
	hdr, size, err := MakeDeltaFile(oldBin, newBin, outPath)
	if err != nil {
		return nil, 0, err
	}
	st := hdr.Stats(size)
	// 只在**确实变小**时才值得发：偶尔（重构、换编译器）delta 可能
	// 比整包还大，那时发布方应直接不发 delta。
	if st.Ratio > 0.5 {
		return hdr, size, fmt.Errorf(
			"delta 过大（%.1f%% of %d bytes）：本版不适合差分发布，请只发整包",
			st.Ratio*100, hdr.NewSize)
	}
	return hdr, size, nil
}

// currentVersionForDelta 读本机内核版本，读不到时返回 nil。
//
// ★ 为什么必须传真实版本给 CheckRelease（而不是 nil）：
// delta 的资产名里带 `-from-<commit8>`，而那个 commit 来自 CurrentC。
// 传 nil 会让 CurrentC 为空 ⇒ 永远匹配不到 delta ⇒ 整条差分路径静默失效
// （看起来"差分已上线"，实际每次都在下整包）。
//
// 读不到版本时返回 nil 是合理的降级：老内核没有 --version，
// 那种情况下也确实不该尝试差分（连基线身份都无法确认）。
func currentVersionForDelta() *BinVersion {
	v, err := ReadBinVersion(KernelBinaryPath())
	if err != nil {
		return nil
	}
	return v
}
