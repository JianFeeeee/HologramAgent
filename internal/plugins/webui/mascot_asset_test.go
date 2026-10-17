package webui

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// 这个小测试钉住一件很具体、也很容易被无声破坏的事：
// **看板娘素材必须自带真 alpha，而不是「白底图」或「棋盘格假透明」。**
//
// 为什么值得一个判据：
//
//	历史上 mascot.webp 是不透明 WebP（白底 + 蓝紫渐变外框）。放在深色面板的
//	30px 圆形头像里，就是一个白方块；site/README.md 里记着因为「没法抠图」
//	最终干脆不放立绘。后来换素材时又出现过「棋盘格 JPEG」—— 那种图**看起来**
//	是透明的，实际像素是灰白格子，直接塞进仓库依然会露出格子。
//
// 这类退化在肉眼 review 里几乎抓不住（缩到 30px 就是一团浅色），但用户每天
// 都在看。所以用容器格式本身来判定：判据看的是 VP8X 的 Alpha 标志位与
// ALPH chunk 是否**一致存在**，而不是「渲染出来像不像透明」。
func TestMascotAssetHasRealAlpha(t *testing.T) {
	data, err := dashboardFS.ReadFile("mascot.webp")
	if err != nil {
		t.Fatalf("读 mascot.webp: %v", err)
	}

	info, err := parseWebPInfo(data)
	if err != nil {
		t.Fatalf("mascot.webp 不是合法 WebP: %v", err)
	}

	if !info.hasAlphaFlag {
		t.Errorf("mascot.webp 的 VP8X Alpha 标志位未置位（flags=0x%02x）——"+
			"说明这张图没有真 alpha 通道，深色面板下会露出白底/棋盘格", info.vp8xFlags)
	}
	if !info.hasALPHChunk {
		t.Errorf("mascot.webp 缺 ALPH chunk —— alpha 标志位与实际数据不一致")
	}

	// 尺寸：30px 圆形头像在 2x 屏上需要 60px；留够余量但别把大图塞进二进制。
	if info.width != info.height {
		t.Errorf("mascot.webp 应为正方形（圆形头像裁剪依赖它），实际 %dx%d", info.width, info.height)
	}
	if info.width < 128 {
		t.Errorf("mascot.webp 只有 %dpx，2x/3x 屏上会糊（要求 >= 128）", info.width)
	}
	if len(data) > 128<<10 {
		t.Errorf("mascot.webp 体积 %.1fKB 超过 128KB —— 它进二进制、每次请求都发，"+
			"头像用途不需要这个量级", float64(len(data))/1024)
	}
}

// 反向验证：把真实素材「退化」成无 alpha 的容器，确认判据会报红。
//
// 这条来自实测教训：形状类判据如果只喂真实数据，它可能对任何输入都绿。
// 这里不依赖外部工具，直接构造一个带 VP8X 但不置 Alpha 位的最小容器。
func TestMascotAssetHasRealAlpha_RejectsOpaqueContainer(t *testing.T) {
	var chunks bytes.Buffer
	payload := make([]byte, 10)         // flags=0x00：Alpha 位为 0
	payload[4], payload[5] = 0x7f, 0x00 // 宽 = 0x7f+1 = 128
	payload[7], payload[8] = 0x7f, 0x00 // 高 = 0x7f+1 = 128
	writeChunk(&chunks, "VP8X", payload)

	info, err := parseWebPInfo(buildRIFF(chunks.Bytes()))
	if err != nil {
		t.Fatalf("构造的容器应能被解析: %v", err)
	}
	if info.hasAlphaFlag {
		t.Fatal("容器本来就声明不透明，解析器却报了 Alpha —— 判据会对退化素材失效")
	}
	if info.hasALPHChunk {
		t.Fatal("构造的容器里没有 ALPH chunk，解析器却说有")
	}
}

type webpInfo struct {
	width, height int
	vp8xFlags     byte
	hasAlphaFlag  bool
	hasALPHChunk  bool
}

var errNotWebP = errors.New("不是 RIFF/WEBP 容器")

// parseWebPInfo 只解析容器头，不解码像素 —— 判据要看的是「格式声明」，
// 不是「看起来透明」。纯标准库即可，不给依赖加负担。
func parseWebPInfo(data []byte) (*webpInfo, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, errNotWebP
	}
	info := &webpInfo{}
	// 遍历 RIFF chunk：4 字节 ID + 4 字节 LE 长度 + 负载（按 2 字节对齐）。
	for off := 12; off+8 <= len(data); {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		payload := off + 8
		if payload+size > len(data) {
			break
		}
		switch id {
		case "VP8X":
			if size >= 10 {
				info.vp8xFlags = data[payload]
				info.hasAlphaFlag = info.vp8xFlags&0x10 != 0 // bit 4 = Alpha
				// 画布尺寸存的是「实际值 - 1」，各 3 字节 LE。
				info.width = int(readUint24LE(data[payload+4:])) + 1
				info.height = int(readUint24LE(data[payload+7:])) + 1
			}
		case "ALPH":
			info.hasALPHChunk = true
		case "VP8 ":
			// 无损/有损的「简单格式」（无 VP8X）：宽高在帧头里。
			// 这种文件必然没有 alpha —— 正是判据要拒的那种。
			if size >= 10 && data[payload+3] == 0x9d && data[payload+4] == 0x01 && data[payload+5] == 0x2a {
				info.width = int(binary.LittleEndian.Uint16(data[payload+6:]) & 0x3FFF)
				info.height = int(binary.LittleEndian.Uint16(data[payload+8:]) & 0x3FFF)
			}
		case "VP8L":
			if size >= 5 {
				// 无损格式：signature 0x2f + 14bit 宽 + 14bit 高 + alpha_is_used 位
				b := data[payload+1 : payload+5]
				info.width = int(binary.LittleEndian.Uint16(b)&0x3FFF) + 1
				info.height = int(binary.LittleEndian.Uint16(b[2:])&0x3FFF) + 1
				if data[payload+4]&0x10 != 0 {
					info.hasAlphaFlag = true
				}
			}
		}
		adv := payload + size
		if size%2 == 1 {
			adv++ // RIFF 要求 chunk 负载补齐到偶数
		}
		off = adv
	}
	if info.width == 0 || info.height == 0 {
		return nil, errNotWebP
	}
	return info, nil
}

func readUint24LE(b []byte) int {
	return int(b[0]) | int(b[1])<<8 | int(b[2])<<16
}

func writeChunk(buf *bytes.Buffer, id string, payload []byte) {
	buf.WriteString(id)
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(payload)))
	buf.Write(sz[:])
	buf.Write(payload)
	if len(payload)%2 == 1 {
		buf.WriteByte(0)
	}
}

func buildRIFF(chunks []byte) []byte {
	var out bytes.Buffer
	out.WriteString("RIFF")
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(4+len(chunks)))
	out.Write(sz[:])
	out.WriteString("WEBP")
	out.Write(chunks)
	return out.Bytes()
}
