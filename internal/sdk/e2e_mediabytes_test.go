//go:build e2e

package sdk

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// 真实数据 e2e：CAS 淘汰后插件经 MediaBytes 取回的字节必须与回填库中
// 块 data 的 sha256 一致（也就是说：插件真拿到了那张图，不是空/错字节）。
func TestE2EMediaBytesAgainstBackfilledDB(t *testing.T) {
	dbPath := os.Getenv("E2E_DB")
	if dbPath == "" {
		t.Skip("E2E_DB 未设置")
	}
	g, err := memory.NewGraphDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	m := NewGraphMemoryWithPlugin("e2e", g)

	ids, err := g.SampleFilledBlockIDs(10)
	if err != nil || len(ids) == 0 {
		t.Skipf("没有已回填块: %v", err)
	}
	ok := 0
	for _, id := range ids {
		want, err := g.BlockData(id)
		if err != nil || len(want) == 0 {
			t.Errorf("BlockData(%s): %v", id, err)
			continue
		}
		wantSum := sha256.Sum256(want)
		data, mime, got, err := m.MediaBytes(id)
		if err != nil || !got {
			t.Errorf("MediaBytes(%s): ok=%v err=%v", id, got, err)
			continue
		}
		gotSum := sha256.Sum256(data)
		if hex.EncodeToString(gotSum[:]) != hex.EncodeToString(wantSum[:]) {
			t.Errorf("%s 字节不一致", id)
			continue
		}
		if mime == "" {
			t.Errorf("%s MIME 为空", id)
		}
		ok++
	}
	t.Logf("MediaBytes e2e：%d/%d 块字节与块 data 完全一致", ok, len(ids))
	if ok == 0 {
		t.Fatal("一个块都没能通过 MediaBytes 取回")
	}
}
