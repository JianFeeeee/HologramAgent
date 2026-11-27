package knowledgeplugin

// scratch.go —— 写面的大正文落盘（见 http.go 的 writeRequest.BodyFile）。
//
// ## 为什么大正文走临时文件
//
// 用户裁定（§九）：「大正文走临时文件传路径」。理由是两个真实的约束：
//
//	1. 知识条目正文可能几十上百 KB（一本教材章节）。塞进 JSON 请求体会让
//	   内存峰值与解析成本随正文线性增长，而 HTTP 面本来就要过一遍网络。
//	2. 外部 agent 手上往往已经有文件（它刚生成/下载的内容），
//	   再让它在 JSON 里转义一遍纯属浪费。
//
// ## 安全边界（不能变成任意文件读取）
//
// 路径必须落在**本进程的临时目录**内（`os.TempDir()/knowledge-scratch`），
// 且拒绝 `..` 与符号链接逃逸。理由：写面虽然是本机默认关闭的，
// 但一旦打开，任意路径读取就等于把整机文件系统暴露给了带 token 的调用方。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxScratchBytes 是允许的单条正文上限（防止一个请求把机器写满）。
const maxScratchBytes = 16 << 20 // 16MB

// scratchDir 返回本插件的临时目录（按需创建）。
func scratchDir() (string, error) {
	dir := filepath.Join(os.TempDir(), "knowledge-scratch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("knowledge scratch: 创建临时目录: %w", err)
	}
	return dir, nil
}

// readScratch 读一个大正文临时文件（**限定在 scratch 目录内**）。
func readScratch(path string) (string, error) {
	dir, err := scratchDir()
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(path)
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("knowledge scratch: 路径不允许包含 ..")
	}
	// 相对路径按 scratch 目录解释（调用方不必知道绝对路径）。
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(dir, clean)
	}
	// 必须仍在 scratch 目录内（abs 比较，防 ../ 与软链逃逸）。
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absPath, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("knowledge scratch: 只接受 %s 内的路径（收到 %s）", absDir, absPath)
	}
	st, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("knowledge scratch: 读不到 %s: %w", absPath, err)
	}
	if st.Size() > maxScratchBytes {
		return "", fmt.Errorf("knowledge scratch: 正文 %d 字节超过上限 %d", st.Size(), maxScratchBytes)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("knowledge scratch: 读 %s: %w", absPath, err)
	}
	return string(data), nil
}
