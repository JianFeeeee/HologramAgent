module github.com/JianFeeeee/HologramAgent

go 1.25.0

require (
	github.com/mattn/go-sqlite3 v1.14.49
	github.com/yuin/gopher-lua v1.1.2
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/yanyiwu/gojieba v1.4.7

require github.com/yalue/onnxruntime_go v1.13.0

require (
	github.com/JianFeeeee/hologramagentsdk v0.9.2
	github.com/charmbracelet/bubbles v1.0.0
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/lipgloss v1.1.0
	golang.org/x/sys v0.38.0
	golang.org/x/text v0.3.8
)

require (
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/charmbracelet/colorprofile v0.4.1 // indirect
	github.com/charmbracelet/x/ansi v0.11.6 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.9.0 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.5.0 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
)

// SDK 仓 module 已改名为 hologramagentsdk（与 GitHub 仓名一致）。
//
// replace 指向本地源码副本，使主仓改动与 SDK 改动可在同一工作树内联调。
// ★ 只能有这一条：Go 不允许同一目录被两个 module 路径 replace
// （报 `used for two different module paths`）。所以主仓**所有** import 都已
// 统一改为新名，旧的 homeagentsdk 路径不再保留。

replace github.com/JianFeeeee/hologramagentsdk => ./third_party/homeagent-sdk
