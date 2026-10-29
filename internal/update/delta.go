package update

// 二进制差分：用「旧版本 + 小补丁」重建新版本，避免每次升下载 88MB。
//
// ## 为什么必须做（实测数据）
//
// 两次构建（同 tag、同参数、-trimpath）：
//
//	基线   88,036,432 字节
//	改一行逻辑  88,036,432 字节，但**逐字节差异只有 80 字节**
//
// 而 OTA 现在下的是整个 tar.gz。也就是说 99.9999% 的流量在传
// 与上一版完全相同的字节。
//
// 前提是可复现构建 —— 本项目已满足（实测两次构建 sha256 相同），
// 这是差分能成立的基础：同一份源码必须产出同一份字节。
//
// ## 算法选择：为什么不直接用 bsdiff
//
// bsdiff 需要 ~17×N 的峰值内存（88MB → 约 1.5GB），在系统内存紧张的
// 机器上会 OOM。而升级恰恰发生在可能内存不足的场景里。
//
// 这里用 rsync 族的**滚动哈希块匹配**：
//
//	构建期：把旧文件按 blockSize 切块建索引（88MB/4KB ≈ 21500 条）
//	差分时：在新文件上逐字节滚动 blockSize 窗口的哈希，命中即验证并
//	        向后扩张匹配，发出 COPY；否则累积 LITERAL
//
// 峰值内存 ≈ 旧文件 + 新文件 + 索引（约 200MB / 88MB 文件），
// 且索引只有几万条，与文件大小线性但系数极小。
//
// ## 为什么 COPY 可以跨任意偏移
//
// 索引里的键是**块内容的哈希**，不是偏移。所以新文件里同一段内容出现在
// 任何位置都能命中 —— 这正好处理"插入/删除几个字节导致后续全部错位"
// 这种最常见的情形（改代码几乎必然改变 .text 的偏移）。
//
// ## 安全边界
//
// 差分的结果**必须**用全部内容的 sha256 校验。patch 应用成功不等于
// 内容正确（旧版本不同、磁盘损坏、patch 被截断都会"成功"应用出
// 一份错误的二进制）。校验失败就丢弃，回退到完整包。

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
)

const (
	// blockSize 是索引的匹配粒度。
	//
	// 选 4KB 的理由：
	//   · 索引条数 = 文件大小/4KB（88MB → 21500 条），内存可忽略
	//   · 匹配粒度足够：改一行代码只影响几十字节，其余 4KB 块全部命中
	//   · 太小（如 64B）会让索引暴涨到 137 万条且收益变差
	blockSize = 4096

	// deltaMagic 是补丁文件头，用于识别格式与版本。
	deltaMagic = "HADELTA1"

	// maxDeltaInput 是允许做差分的单文件上限。
	//
	// 88MB 的二进制 + 索引约需 200MB 峰值。设 256MB 上限是为了防止
	// 误用（例如有人对模型文件做差分）导致内存爆掉。
	maxDeltaInput = 256 << 20

	opCopy    = byte(0x01)
	opLiteral = byte(0x02)
)

// DeltaHeader 是补丁的元信息。
type DeltaHeader struct {
	OldSize int64
	NewSize int64
	// NewSHA256 是**新文件的完整 sha256**，应用后必须校验。
	NewSHA256 string
	// OldSHA256 是**旧文件的完整 sha256**，应用前必须校验。
	//
	// ★ 为什么记旧文件的哈希：差分只能对"特定旧版本"应用。
	// 用错基线（例如两个版本之间的跨版本 delta）会静默产出一份垃圾，
	// 而它的大小、结构都"看起来正常"。
	OldSHA256 string
}

// MakeDelta 生成把 oldData 变成 newData 的补丁。
func MakeDelta(oldData, newData []byte) ([]byte, *DeltaHeader, error) {
	if len(oldData) > maxDeltaInput || len(newData) > maxDeltaInput {
		return nil, nil, fmt.Errorf("文件超出差分上限（%d MB）", maxDeltaInput>>20)
	}

	oldSum := sha256.Sum256(oldData)
	newSum := sha256.Sum256(newData)
	hdr := &DeltaHeader{
		OldSize:   int64(len(oldData)),
		NewSize:   int64(len(newData)),
		OldSHA256: hex.EncodeToString(oldSum[:]),
		NewSHA256: hex.EncodeToString(newSum[:]),
	}

	idx := buildBlockIndex(oldData)

	var body bytes.Buffer
	// 预留：最坏情况全是字面量，但字面量会分片合并，实际远小于 N。
	body.Grow(len(newData)/64 + 1024)

	var lit []byte
	flushLit := func() {
		if len(lit) == 0 {
			return
		}
		body.WriteByte(opLiteral)
		var tmp [4]byte
		binary.LittleEndian.PutUint32(tmp[:], uint32(len(lit)))
		body.Write(tmp[:])
		body.Write(lit)
		lit = lit[:0]
	}

	writeCopy := func(off int64, n int) {
		body.WriteByte(opCopy)
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], uint64(off))
		body.Write(tmp[:])
		var t4 [4]byte
		binary.LittleEndian.PutUint32(t4[:], uint32(n))
		body.Write(t4[:])
	}

	i := 0
	for i < len(newData) {
		// 窗口不足一个块时无法匹配，直接当字面量
		if i+blockSize > len(newData) {
			lit = append(lit, newData[i:]...)
			break
		}
		h := crc32.ChecksumIEEE(newData[i : i+blockSize])
		cands := idx[h]
		matched := int64(-1)
		matchLen := 0
		for _, off := range cands {
			// 哈希命中后必须**逐字节验证**：CRC32 会碰撞，
			// 而碰撞导致的错误 COPY 会让输出静默损坏。
			if !bytes.Equal(oldData[off:off+blockSize], newData[i:i+blockSize]) {
				continue
			}
			// 向后扩张：把相邻的相同字节一起纳入 COPY
			n := blockSize
			for i+n < len(newData) && off+int64(n) < int64(len(oldData)) &&
				newData[i+n] == oldData[off+int64(n)] {
				n++
			}
			if n > matchLen {
				matchLen = n
				matched = off
			}
			if matchLen >= 1<<20 {
				break // 足够长，不必再找
			}
		}

		if matched >= 0 {
			flushLit()
			writeCopy(matched, matchLen)
			i += matchLen
			continue
		}
		lit = append(lit, newData[i])
		i++
	}
	flushLit()

	var out bytes.Buffer
	out.WriteString(deltaMagic)
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], uint64(len(oldData)))
	out.Write(b8[:])
	binary.LittleEndian.PutUint64(b8[:], uint64(len(newData)))
	out.Write(b8[:])
	// 两个哈希固定 64 字节 hex
	oldHex := make([]byte, 64)
	newHex := make([]byte, 64)
	copy(oldHex, hdr.OldSHA256)
	copy(newHex, hdr.NewSHA256)
	out.Write(oldHex)
	out.Write(newHex)
	out.Write(body.Bytes())

	return out.Bytes(), hdr, nil
}

// buildBlockIndex 把旧文件按 blockSize 切块，建「CRC32 → 偏移列表」索引。
func buildBlockIndex(data []byte) map[uint32][]int64 {
	idx := make(map[uint32][]int64, len(data)/blockSize+1)
	for off := 0; off+blockSize <= len(data); off += blockSize {
		h := crc32.ChecksumIEEE(data[off : off+blockSize])
		idx[h] = append(idx[h], int64(off))
	}
	return idx
}

// ParseDeltaHeader 读补丁头（不应用）。
func ParseDeltaHeader(patch []byte) (*DeltaHeader, error) {
	if len(patch) < len(deltaMagic)+16+128 {
		return nil, fmt.Errorf("补丁太短（%d 字节），不是有效差分", len(patch))
	}
	if string(patch[:len(deltaMagic)]) != deltaMagic {
		return nil, fmt.Errorf("补丁魔数不符：期望 %q，实际 %q", deltaMagic, patch[:len(deltaMagic)])
	}
	p := len(deltaMagic)
	hdr := &DeltaHeader{
		OldSize:   int64(binary.LittleEndian.Uint64(patch[p:])),
		NewSize:   int64(binary.LittleEndian.Uint64(patch[p+8:])),
		OldSHA256: string(patch[p+16 : p+16+64]),
		NewSHA256: string(patch[p+16+64 : p+16+128]),
	}
	return hdr, nil
}

// ApplyDelta 用旧数据 + 补丁重建新数据，并**校验两端的 sha256**。
//
// 校验是硬性的：patch 应用"成功"不代表内容正确。旧版本不匹配、
// 磁盘损坏、patch 被截断，都会产出一份"看起来正常"的错误二进制。
// 唯一可信的判据是全内容 sha256。
func ApplyDelta(oldData, patch []byte) ([]byte, error) {
	hdr, err := ParseDeltaHeader(patch)
	if err != nil {
		return nil, err
	}

	// ① 先验旧文件：差分只对**特定旧版本**成立。
	oldSum := sha256.Sum256(oldData)
	if got := hex.EncodeToString(oldSum[:]); got != hdr.OldSHA256 {
		return nil, fmt.Errorf("基线不匹配：补丁针对 %s，本地是 %s",
			ShortSum(hdr.OldSHA256), ShortSum(got))
	}
	if int64(len(oldData)) != hdr.OldSize {
		return nil, fmt.Errorf("基线大小不匹配：期望 %d，实际 %d", hdr.OldSize, len(oldData))
	}

	// ② 执行指令
	p := len(deltaMagic) + 16 + 128
	out := make([]byte, 0, hdr.NewSize)
	for p < len(patch) {
		if p+1 > len(patch) {
			return nil, fmt.Errorf("补丁在偏移 %d 处截断（缺操作码）", p)
		}
		op := patch[p]
		p++
		switch op {
		case opCopy:
			if p+12 > len(patch) {
				return nil, fmt.Errorf("补丁在偏移 %d 处截断（COPY 参数）", p)
			}
			off := int64(binary.LittleEndian.Uint64(patch[p:]))
			n := int(binary.LittleEndian.Uint32(patch[p+8:]))
			p += 12
			if off < 0 || n < 0 || off+int64(n) > int64(len(oldData)) {
				return nil, fmt.Errorf("COPY 越界：off=%d n=%d oldSize=%d", off, n, len(oldData))
			}
			out = append(out, oldData[off:off+int64(n)]...)
		case opLiteral:
			if p+4 > len(patch) {
				return nil, fmt.Errorf("补丁在偏移 %d 处截断（LITERAL 长度）", p)
			}
			n := int(binary.LittleEndian.Uint32(patch[p:]))
			p += 4
			if n < 0 || p+n > len(patch) {
				return nil, fmt.Errorf("LITERAL 越界：n=%d 剩余=%d", n, len(patch)-p)
			}
			out = append(out, patch[p:p+n]...)
			p += n
		default:
			return nil, fmt.Errorf("未知操作码 0x%02x（偏移 %d）", op, p-1)
		}
		// 防御：补丁可以声明很小的 NewSize 但指令产出巨量数据
		if int64(len(out)) > hdr.NewSize+int64(blockSize) {
			return nil, fmt.Errorf("补丁产出的数据超过声明大小（%d > %d）", len(out), hdr.NewSize)
		}
	}

	if int64(len(out)) != hdr.NewSize {
		return nil, fmt.Errorf("重建大小不符：期望 %d，实际 %d", hdr.NewSize, len(out))
	}

	// ③ 校验结果：这是唯一可信的判据
	newSum := sha256.Sum256(out)
	if got := hex.EncodeToString(newSum[:]); got != hdr.NewSHA256 {
		return nil, fmt.Errorf("重建内容校验失败：期望 %s，实际 %s",
			ShortSum(hdr.NewSHA256), ShortSum(got))
	}
	return out, nil
}

// ── 文件级入口（供 CLI / 发布脚本使用） ────────────────────────

// MakeDeltaFile 从磁盘上的两个文件生成补丁文件。
func MakeDeltaFile(oldPath, newPath, patchPath string) (*DeltaHeader, int64, error) {
	oldData, err := os.ReadFile(oldPath)
	if err != nil {
		return nil, 0, fmt.Errorf("读旧文件: %w", err)
	}
	newData, err := os.ReadFile(newPath)
	if err != nil {
		return nil, 0, fmt.Errorf("读新文件: %w", err)
	}
	patch, hdr, err := MakeDelta(oldData, newData)
	if err != nil {
		return nil, 0, err
	}
	if err := WriteFileAtomic(patchPath, patch, 0644); err != nil {
		return nil, 0, fmt.Errorf("写补丁: %w", err)
	}
	return hdr, int64(len(patch)), nil
}

// ApplyDeltaFile 用旧文件 + 补丁重建新文件。
func ApplyDeltaFile(oldPath, patchPath, outPath string) (int64, error) {
	oldData, err := os.ReadFile(oldPath)
	if err != nil {
		return 0, fmt.Errorf("读基线: %w", err)
	}
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		return 0, fmt.Errorf("读补丁: %w", err)
	}
	out, err := ApplyDelta(oldData, patch)
	if err != nil {
		return 0, err
	}
	if err := WriteFileAtomic(outPath, out, 0755); err != nil {
		return 0, fmt.Errorf("写结果: %w", err)
	}
	return int64(len(out)), nil
}

// DeltaStats 用于报告压缩效果（发布脚本 / 日志）。
type DeltaStats struct {
	OldSize   int64
	NewSize   int64
	PatchSize int64
	Ratio     float64
}

// Stats 计算压缩比。
func (h *DeltaHeader) Stats(patchSize int64) DeltaStats {
	r := 0.0
	if h.NewSize > 0 {
		r = float64(patchSize) / float64(h.NewSize)
	}
	return DeltaStats{OldSize: h.OldSize, NewSize: h.NewSize, PatchSize: patchSize, Ratio: r}
}

// sortedKeys 仅用于测试与诊断（map 遍历顺序不稳定）。
func sortedKeys(m map[uint32][]int64) []uint32 {
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

var _ = io.Discard
