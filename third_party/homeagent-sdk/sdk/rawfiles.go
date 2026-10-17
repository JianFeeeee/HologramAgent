package sdk

// RawFilesAPI 让插件读写**随包分发的资源**（rawfile）。
//
// ## 是什么
//
// `hmapdev build` 会把工程根 `rawfile/` 下的全部内容（以及 plg.json
// `rawfiles` 声明的外部路径）打进 .hmap；内核解包时保留路径，落到：
//
//	<插件目录>/rawfile/<原相对路径>
//
// 本接口就是插件读这些资源的唯一入口。
//
// ## 为什么不让插件自己拼路径
//
// 插件**不知道自己的安装目录**：内核的 `PluginDir()` 是给插件管理器用的，
// 不暴露给插件自身。而且"包内资源"与"运行期生成的文件"必须分开 ——
// 前者只读、随升级替换，后者可写、跨升级保留。混在一个目录里，
// 升级时会误删运行期数据，或把旧资源留在原地。
//
// 所以：
//
//	RawFiles().Read("main/prompt.txt")   → 读**包内**资源（只读）
//	RawFiles().WorkDir()                 → 拿**可写**工作目录（运行期数据放这）
//
// ## 安全边界
//
// 所有路径都相对 rawfile/ 根，**必须拒绝路径穿越**（`../`）。
// 资源文件来自包，包可能被替换 —— 让插件能借它读到 `/etc/shadow`
// 就是一个真实的提权面。
type RawFilesAPI interface {
	// Read 读一个包内资源（路径相对 rawfile/ 根）。
	//
	// 例：Read("main/prompt.txt") 读 <插件目录>/rawfile/main/prompt.txt。
	Read(rel string) ([]byte, error)

	// List 列出某个目录下的条目（相对 rawfile/ 根；"" 表示根）。
	//
	// 返回的名字是**相对 path 的单层名**（不是全路径），目录名带 `/` 后缀。
	List(rel string) ([]string, error)

	// Exists 报告某个资源是否存在。
	Exists(rel string) bool

	// Dir 返回包内资源的根目录（只读）。
	//
	// 给需要把整个目录交给外部工具的插件用（如把 node 目录的路径
	// 传给 exec.Command）。**不要往这里写东西** —— 它在包里，随升级替换。
	Dir() string

	// WorkDir 返回一个**可写**目录，供插件放运行期生成的文件。
	//
	// 与 Dir() 分开的原因：Dir() 里的东西随升级被整体替换，
	// 运行期数据放那里会在下次升级时丢失。
	WorkDir() string
}
