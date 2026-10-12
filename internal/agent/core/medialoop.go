package core

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
)

// 媒体与记忆块的生命周期辅助。
//
// ★ 2026-10-11：块自带负载（media.Store/CAS 已淘汰）。
//
// 此前这里有 heldMediaDigests / payloadHeld / forgetPayloads 三个函数，
// 它们存在的**唯一理由**是补 CAS 的漏：CAS 是全库字节存储，它的检索结果
// 不等于「记忆里的媒体」，所以要靠一套 digest 记账把已无处可归的内容滤掉。
// 块自带负载后这一整类问题从根上消失——字节就在块里，块没了字节就没了，
// 不需要账本、不需要 GC、不需要「删前先问谁还持有」。
//
// 因此那三个函数被整体删除，只保留向量迁移（它仍需要，因为换模型/换维度
// 会让历史块的向量落在另一个空间里）。

// reembedStaleMedia 在启动时批量把历史媒体块的向量迁移到当前向量空间。
//
// 触发场景（任一变化都会导致旧向量无法参与查询）：
//   - 切换模型（模型 A→模型 B，fp 变了）
//   - 切换向量维度（ONNX→HTTP dim 512→1024）
//   - 首次部署嵌入服务（历史无向量的媒体补算）
//   - 嵌入服务离线后重新上线（失败条目 fingerprint 仍为空）
//
// 并发策略：启动时用 worker pool 并行迁移，避免上千张图片串行耗时过长。
// 并发数在 ONNX 内嵌路径下不超 CPU 核心数（避免 ONNX 并发限流），
// 外部 API 路径下不超 8（避免打爆外部服务）。
func (a *Agent) reembedStaleMedia() {
	if a.multimodalSpace == nil || !a.multimodalSpace.Loaded() || a.memory == nil {
		return
	}
	fp := a.multimodalSpace.Fingerprint()
	stale, err := a.memory.StaleBlockVectors(fp)
	if err != nil {
		log.Printf("[media] 查询需重算向量的块失败: %v", err)
		return
	}
	// 只迁图像：多模态空间当前覆盖 text+image，音频/视频不在其内
	// （不拿别的模态的向量顶替——那是静默错配）。
	// StaleBlockVectors 只给元数据（lean 列），字节由 reembedOneBlock 按需取。
	var targets []memory.MemoryBlock
	for _, b := range stale {
		if b.Modality == memory.BlockImage {
			targets = append(targets, b)
		}
	}
	if len(targets) == 0 {
		log.Printf("[media] 无需迁移向量（所有图像块已与当前空间对齐 fp=%s）", shortFP(fp))
		return
	}

	workers := 4
	if len(fp) >= 5 && fp[:5] == "http:" {
		workers = 8
	}
	log.Printf("[media] 启动向量迁移: %d 个图像块 → fp=%s dim=%d workers=%d",
		len(targets), shortFP(fp), a.multimodalSpace.Dim(), workers)

	jobs := make(chan memory.MemoryBlock, workers*2)
	var done, failed int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				if err := a.reembedOneBlock(b, fp); err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				atomic.AddInt64(&done, 1)
			}
		}()
	}

	for i, b := range targets {
		jobs <- b
		if (i+1)%20 == 0 {
			log.Printf("[media] 向量迁移进度: %d/%d (done=%d failed=%d)",
				i+1, len(targets), atomic.LoadInt64(&done), failed)
		}
	}
	close(jobs)
	wg.Wait()
	log.Printf("[media] 向量迁移完成: 成功=%d 失败=%d 总计=%d fp=%s",
		done, failed, len(targets), shortFP(fp))
}

// reembedOneBlock 为一个图像块重算向量并写回。负载就在块里，无需外部取字节。
//
// 模态不在本空间覆盖范围时返回 ErrModalityUnsupported，调用方据此区分
// 「永久无向量」与「本次失败重试」。
func (a *Agent) reembedOneBlock(b memory.MemoryBlock, fp string) error {
	mime := b.MIME
	if mime == "" {
		mime = "image/png"
	}
	// 负载不在 lean 列里：按块 ID 单取字节（块即 union）。
	data := b.Data
	if len(data) == 0 {
		var err error
		data, err = a.memory.BlockData(b.ID)
		if err != nil {
			return err
		}
	}
	if len(data) == 0 {
		return fmt.Errorf("block %s has no payload", b.ID)
	}
	vec, err := a.multimodalSpace.EmbedImageDense(data, mime)
	if err != nil {
		return err
	}
	if err := a.memory.SetBlockVector(b.ID, vec, fp); err != nil {
		return err
	}
	return nil
}

// shortFP 截断 fingerprint 为可读日志格式。
func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}
