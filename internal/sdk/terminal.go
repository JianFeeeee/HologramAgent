package sdk

// 终端 DTO（TerminalStatus / CmdExecStatus）保留：它们只是**线上格式**，
// WebUI 与 CLI 用同一口径返回给前端。终端语义（谁在跑、历史怎么归并）
// 已归属 agentcli 与插件侧视图（internal/pluginview），内核不再有 TerminalAPI。
// TerminalStatus 与 WebUI termState / CLI cliTermState 同一 JSON 口径。
type TerminalStatus struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Running   bool   `json:"running"`
	Output    string `json:"output"`
	CreatedAt string `json:"created_at"`
	Uptime    string `json:"uptime"`
}

// CmdExecStatus 与 WebUI CmdExec 同一 JSON 口径。
type CmdExecStatus struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Status   string `json:"status"`
	Time     string `json:"time"`
}
