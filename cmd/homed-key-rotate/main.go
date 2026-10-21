// homed-key-rotate：主密钥轮换（E-1）。
//
// ## 为何必须有这个命令
//
// `security` 包里的 `RotateKey` / `RotateKeyFile` / `RekeySecrets` 只是**零件**。
// 没有一条可执行的路径把它们串起来，E-1 就只是库里的能力，而不是
// 运维真能做的事 —— 而轮换的价值恰恰在于「出事时能执行」。
//
// ## 做三件事，顺序不可换
//
//	① 重加密变量库（security/variables.json）→ 新钥
//	② 写 security.keyring（旧钥降为只读历史）
//	③ 重加密 config.db 里的密文
//
// ★ 为什么必须「先重加密、后写 keyring」：
// 反过来的话，进程在中间崩溃会留下「keyring 说当前是新钥，但数据还是旧钥密文」
// 的状态 —— 而旧钥已不在环里可自动恢复，等于数据全丢。
//
// ## 用法
//
//	homed-key-rotate -data /var/lib/homeagent -new-key <64位hex>
//	homed-key-rotate -data /var/lib/homeagent -gen   # 生成一把新钥并打印
//
// 轮换后**必须重启 homed**（密钥在启动时装配）。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

func main() {
	dataDir := flag.String("data", "", "数据目录（config.db 所在处）")
	newKeyHex := flag.String("new-key", "", "新主密钥（64 位 hex）；与 -gen 二选一")
	gen := flag.Bool("gen", false, "生成一把新主密钥并打印（不执行轮换）")
	flag.Parse()

	if *gen {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			fail("生成密钥失败: %v", err)
		}
		fmt.Println(hex.EncodeToString(k))
		return
	}
	if *dataDir == "" || *newKeyHex == "" {
		fail("必须给 -data 与 -new-key（或 -gen 生成一个）")
	}

	// ① 先取当前钥（走与 homed 完全相同的优先级，避免拿错钥）
	oldKey, src, err := security.LoadOrCreateKey(*dataDir)
	if err != nil {
		fail("读取当前主密钥失败: %v", err)
	}
	newKey, err := hex.DecodeString(*newKeyHex)
	if err != nil || len(newKey) != 32 {
		fail("-new-key 必须是 64 位 hex（32 字节）")
	}
	if string(newKey) == string(oldKey) {
		fail("新钥与当前钥相同，无需轮换")
	}
	history, err := security.LoadKeyring(*dataDir, oldKey)
	if err != nil {
		fail("读取密钥环失败: %v", err)
	}
	fmt.Printf("当前密钥来源: %s（历史密钥 %d 把）\n", src, len(history))

	// ② 重加密变量库
	st := security.New(security.Options{
		Path:    filepath.Join(*dataDir, "security", "variables.json"),
		KeyFn:   func() ([]byte, error) { return oldKey, nil },
		OldKeys: history,
		Enabled: true,
	})
	if err := st.Load(); err != nil {
		fail("加载变量库失败（不静默重建）: %v", err)
	}
	n := st.Count()
	if err := st.RotateKey(newKey); err != nil {
		fail("变量库轮换失败（数据未改动）: %v", err)
	}
	fmt.Printf("① 变量库已重加密（%d 条）\n", n)

	// ③ 写 keyring（旧钥转为只读历史）。放在数据重加密之后 —— 见文件头说明。
	if err := security.RotateKeyFile(*dataDir, oldKey, newKey); err != nil {
		fail("写密钥环失败（变量库已是新钥，此时不可回退）: %v", err)
	}
	fmt.Printf("② 密钥环已更新（%s）\n", filepath.Join(*dataDir, security.KeyringFileName))
	// 当前钥来源若是开发/磁盘路径，必须同步替换 security.key；否则下次启动
	// LoadOrCreateKey 仍读旧钥，而 keyring current 已是新钥，会按设计响亮拒绝启动。
	// systemd credential/env 不能由此命令擅自改：必须由运维更新注入源。
	if src == filepath.Join(*dataDir, security.KeyFileName) {
		path := filepath.Join(*dataDir, security.KeyFileName)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(hex.EncodeToString(newKey)), 0o600); err != nil {
			fail("更新磁盘主密钥失败: %v", err)
		}
		if err := os.Chmod(tmp, 0o600); err != nil {
			fail("确权新主密钥失败: %v", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			fail("替换磁盘主密钥失败: %v", err)
		}
		fmt.Printf("③ 磁盘主密钥已替换（%s）\n", path)
	} else {
		fmt.Printf("③ 当前钥来自 %s：请先更新该注入源为新钥，再重启 homed\n", src)
	}
	if _, err := security.InitConfigCodecWithOldKeys(newKey, append([][]byte{oldKey}, history...)); err != nil {
		fail("装配新配置 codec 失败: %v", err)
	}
	reg := config.NewConfigRegistry(filepath.Join(*dataDir, "config.db"))
	defer reg.Close()
	rekeyed, failed, err := reg.RekeySecrets()
	if err != nil {
		fail("config.db 重加密失败: %v", err)
	}
	fmt.Printf("③ config.db 已重加密 %d 条（失败 %d 条）\n", rekeyed, failed)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "⚠ 有 %d 条密文用新旧钥都解不开 —— 那说明它们来自更早的密钥，\n"+
			"  请检查 security.keyring 的 history 是否完整（不要在此状态下删除旧钥）\n", failed)
	}
	fmt.Println("\n完成。请重启 homed 以使新密钥生效：systemctl restart homeagent.service")
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}
