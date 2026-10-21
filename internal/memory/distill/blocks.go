package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// 拆解产物的落库形态：句子 + 文本块 + 结构边。
//
// 为什么不是 entities/relations（本会话最初的错误做法）
// --------------------------------------------------
// 三元组在图里的正确形态是【节点】【关系边】【节点】，而节点已从纯文本实体
// 升级为带 vector/fingerprint/modality 的 memory_blocks（46f833c）。把拆解
// 结果 Commit 成 entities 是把新产出灌进正在退场的旧形态。
//
// 端点 kind 的既有设计（block.go:175 validGraphNodeKind）：
//
//	block / entity / sentence / document
//
// 句子的职责由 sentences 表承担（它已有 id、text、created_at），
// 块的职责是**可向量化的最小子项目**。所以正确的挂接是：
//
//	sentence(原句) --contains--> block(字段内容, 带向量)
//
// 与媒体块完全同构（graphmedia.go:73 的 sentence→block contains 边）。
// 不把原句也做成块 —— 那会与 sentences 表重复，且原句不需要单独向量
// （它的语义由它包含的字段块共同表达）。

// BlockPayload 是一次拆解的产物：原句 + 字段块 + 连接关系。
type BlockPayload struct {
	// Sentence 是原始记录文本（写入 sentences 表，作为结构边的源端点）。
	Sentence string
	// Fields 是按维度拆出的字段块。
	Fields []FieldBlock
}

// FieldBlock 是一个字段块：**所属主语** + 维度名 + 原样值。
//
// ★ Subject 不能省（实测踩出来的）：一句多主语时
// 「admin服务端口8861·billing服务端口8499·oauth服务端口8271」
// 会拆出三条同维度不同值的字段：
//
//	{admin服务, 端口, 8861}  {billing服务, 端口, 8499}  {oauth服务, 端口, 8271}
//
// 少了 Subject 就只剩「端口=8861」，三个服务的端口落库后无法区分 ——
// 查「billing 的端口」时块文本里根本没有 billing。
// 单主语时 Subject 是那条主语（如「值班室分机号」），它同样要进块文本，
// 否则「值班分机号=4324」查不到「值班室分机号是多少」。
type FieldBlock struct {
	// Subject 是这条字段所属的主语。空串表示无主语（不该出现 ——
	// Split 的闸门会挡掉无主语的字段）。
	Subject   string
	Dimension string
	Value     string
}

// Blocks 把一条记录拆成块形态。
//
// 与 Split 的区别：Split 返回 Triple（旧形态，供对照期使用）；
// Blocks 返回块形态（新形态）。两者共用同一套拆解逻辑与闸门，
// 不重复实现——闸门（值原样校验、维度归一、长度上限、自环过滤）
// 是拆解质量的全部保障，分两处实现必然漂移。
func (e *Extractor) Blocks(ctx context.Context, record string) (*BlockPayload, error) {
	triples, err := e.Split(ctx, record)
	if err != nil {
		return nil, err
	}
	if len(triples) == 0 {
		return &BlockPayload{Sentence: record}, nil
	}
	payload := &BlockPayload{Sentence: record}
	for _, t := range triples {
		payload.Fields = append(payload.Fields, FieldBlock{
			Subject:   t.Subject,
			Dimension: t.Relation,
			Value:     t.Object,
		})
	}
	return payload, nil
}

// BlockID 由句子与字段内容派生，保证同一记录重复拆解得到同一个块 ID。
//
// 为什么用内容摘要而不是随机 ID：蒸馏是**可重试**的（distillOnce 失败会
// 把记录写回队列下次重试），随机 ID 会让每次重试都产生一批新块，
// 同一句记忆在库里堆成 N 份。内容派生 ID 配合 PutMemoryBlocks 的
// ON CONFLICT 语义天然幂等。
//
// ★ subject 必须参与 ID 派生：三个服务的端口
// 「admin服务端口8861·billing服务端口8499」若不含 subject，
// 「端口=8861」与另一个主语的「端口=8861」会撞成同一个块 ——
// 那是静默的数据丢失（后者被覆盖，且不会有任何报错）。
func BlockID(sentence, subject, dimension, value string) string {
	h := sha256.Sum256([]byte(sentence + "\x00" + subject + "\x00" +
		dimension + "\x00" + value))
	return "blk_" + hex.EncodeToString(h[:12])
}

// SentenceBlockID 由原句文本派生原句块的 ID。
//
// ★ 实现在 memory 包（memory.SentenceBlockID）——
//
//	迁移与蒸馏两条路径都要用它，而 distill 已导入 memory，
//	放在 distill 会形成导入环。这里只做转发以便包内调用方便。
func SentenceBlockID(sentence string) string { return memory.SentenceBlockID(sentence) }

// SentenceBlock 构造承载原句的块（转发到 memory.NewSentenceBlock）。
//
// 刻意不带向量：原句块是溯源锚点，长整句会稀释向量召回。
func SentenceBlock(sentence string) memory.MemoryBlock {
	return memory.NewSentenceBlock(sentence, time.Time{}, time.Time{})
}

// ToMemoryBlock 把一个字段块转成可入库的 MemoryBlock。
//
// Vector/Fingerprint 由调用方填充（需要 embedding provider）。**provider
// 不可用时留空而不是编造** —— 无向量的块仍可被 BlocksForNode 按边查到，
// 只是不参与向量召回；编造一个零向量会让它参与检索并永远排在最后，
// 那是静默的错误记忆。
func (f FieldBlock) ToMemoryBlock(sentence string) memory.MemoryBlock {
	// 块文本形态：<主语>|<维度>=<值>
	//
	// ★ 主语必须进文本（不只是进 BlockID）：向量是基于 text_content 算的，
	// 「billing服务 端口=8499」能被「billing 服务的端口是多少」召回，
	// 而只有「端口=8499」的那个块召回不到 —— 三个服务的端口同维度时
	// 光靠维度+值无法区分归属（实测形态见 FieldBlock 的注释）。
	//
	// 分隔符用 '|'（不在维度名/值/主语的字符集里）：维度名已受控词表归一
	// 不含 '|='，主语是实体名同样不含。而文本只有一个 '=' 时
	// Dimension/Value 的切分才可靠，所以主语与维度之间用 '|' 隔开。
	text := f.Dimension + "=" + f.Value
	if f.Subject != "" {
		text = f.Subject + "|" + text
	}
	return memory.MemoryBlock{
		ID:       BlockID(sentence, f.Subject, f.Dimension, f.Value),
		Modality: memory.BlockText,
		Text:     text,
		Source:   "distill",
	}
}

// Dimension 从块文本里取回维度名（'=' 前）。取不到时返回空串。
//
// 用途：召回后要告诉模型「这条记忆是关于哪个维度的」而不只是原文串。
func Dimension(blockText string) string {
	i := strings.Index(blockText, "=")
	if i <= 0 {
		return ""
	}
	// ★ 去掉主语前缀（「<主语>|<维度>=…」形态）。
	// 不去掉的话 Dimension 会返回「billing服务|端口」，
	// 维度名与主语混在一起 —— 而维度名是受控词表归一过的，
	// 混进去之后边就建不起来了。
	if j := strings.LastIndex(blockText[:i], "|"); j >= 0 {
		return blockText[j+1 : i]
	}
	return blockText[:i]
}

// Value 从块文本里取回值（'=' 后）。
func Value(blockText string) string {
	i := strings.Index(blockText, "=")
	if i < 0 {
		return blockText
	}
	return blockText[i+1:]
}

// BlockSubject 从块文本里取回所属主语（首个 '|' 前）。没有则返回空串。
//
// 命名：subject.go 里 Subject 已是推导出的主语类型，这里是「从块文本
// 反解出主语」的函数，不能同名。
//
// 用途：召回后要告诉模型「这条记忆是关于谁的」——
// 一句多主语时（三个服务各自的端口）没有主语就分不清归属。
func BlockSubject(blockText string) string {
	i := strings.Index(blockText, "|")
	if i <= 0 {
		return ""
	}
	return blockText[:i]
}

// WritePayload 把拆解产物写入图：句子行 + 字段块 + contains 边。
//
// embed 为 nil 时块不带向量（不编造）。返回写入的块数。
//
// 顺序与失败语义：句子必须先落（边的一端要存在，AddMemoryBlockEdge 会校验），
// 而 PutMemoryBlocks 本身是事务、AddMemoryBlockEdge 也是事务 —— 这里
// 逐块提交而非整批原子：一次拆解里某个块写失败不该让其余块回滚，
// 因为每条边都是独立事实（块 A 与块 B 之间没有依赖）。
func WritePayload(ctx context.Context, g *memory.GraphDB, payload *BlockPayload,
	embed func(string) ([]float64, string)) (int, error) {
	if g == nil || payload == nil || strings.TrimSpace(payload.Sentence) == "" {
		return 0, fmt.Errorf("distill: empty payload")
	}
	if len(payload.Fields) == 0 {
		return 0, nil
	}

	// ★ 原句写成块，不再依赖 sentences 表（方案 A 的最后依赖点）。
	//
	// 之前：先写 sentences 行，再 sentence --contains--> block
	// 现在：SentenceBlock → 原句块 --contains--> 字段块
	//
	// 两者对召回完全等价（召回只看字段块），差别在退场路径：
	// 前者要保留 sentences 表，后者删掉即可。
	src := SentenceBlock(payload.Sentence)
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{src}); err != nil {
		return 0, fmt.Errorf("distill: put sentence block: %w", err)
	}

	written := 0
	for _, f := range payload.Fields {
		b := f.ToMemoryBlock(payload.Sentence)
		if embed != nil {
			if vec, fp := embed(b.Text); len(vec) > 0 {
				b.Vector = vec
				b.Fingerprint = fp
			}
		}
		if err := g.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
			return written, fmt.Errorf("distill: put block %s: %w", b.ID, err)
		}
		if err := g.AddMemoryBlockEdge("block", src.ID,
			"block", b.ID, "contains"); err != nil {
			return written, fmt.Errorf("distill: link block %s: %w", b.ID, err)
		}
		written++
	}
	return written, nil
}
