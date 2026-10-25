package memory

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yanyiwu/gojieba"
)

// contentPOS 有实义的词性标签：只保留名词/动词/形容词/专名等
var contentPOS = map[string]bool{
	"n":   true, // 普通名词
	"nr":  true, // 人名
	"ns":  true, // 地名
	"nt":  true, // 机构名
	"nw":  true, // 作品名/URL
	"nz":  true, // 其他专名
	"v":   true, // 动词
	"vd":  true, // 副动词
	"vn":  true, // 名动词
	"a":   true, // 形容词
	"ad":  true, // 副形词
	"an":  true, // 名形词
	"i":   true, // 成语
	"l":   true, // 习用语
	"j":   true, // 简称
	"s":   true, // 处所词
	"f":   true, // 方位词
	"b":   true, // 区别词
	"z":   true, // 状态词
	"t":   true, // 时间词
	"eng": true, // 英文
	"x":   true, // 非语素字
	"zg":  true, // 其他
	"un":  true, // 未知词性——保守保留
}

var (
	jiebaOnce sync.Once
	jiebaInst *gojieba.Jieba
)

func GetJieba() *gojieba.Jieba {
	jiebaOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[jieba] init panic recovered: %v", r)
			}
		}()
		d := jiebaDictDir()
		if d == "" {
			log.Printf("[jieba] dictionary directory not found (embedded extraction failed and no module cache), jieba disabled")
			return
		}
		jiebaInst = gojieba.NewJieba(
			filepath.Join(d, "jieba.dict.utf8"),
			filepath.Join(d, "hmm_model.utf8"),
			filepath.Join(d, "user.dict.utf8"),
			filepath.Join(d, "idf.utf8"),
			filepath.Join(d, "stop_words.utf8"),
		)
	})
	return jiebaInst
}

func jiebaDictDir() string {
	// 首选内嵌词库：它是产物的一部分，与二进制同版本、不依赖宿主环境。
	//
	// 以前这里只猜 GOMODCACHE/GOPATH/~/go/pkg/mod，部署机上通常没有 Go 模块缓存，
	// 于是分词与关键词提取会**静默退回空列表**（详见 jieba_embed.go 的说明）。
	if dir, err := materializeJiebaDict(); err == nil && dir != "" {
		return dir
	}
	log.Printf("[jieba] writing embedded dictionary failed, falling back to module cache lookup (embedded failure usually means cache dir is not writable)")

	// 回退：开发机上存在的模块缓存（仅作为兵底，不应依赖它）。
	//
	// GOMODCACHE is typically $GOPATH/pkg/mod. When set, Go writes modules
	// under <GOMODCACHE>/github.com/... . Look first at GOMODCACHE, then
	// derive from GOPATH, then try common locations.
	return jiebaDictDirFromModuleCache()
}

func jiebaDictDirFromModuleCache() string {
	candidates := []string{
		os.Getenv("GOMODCACHE"),
	}
	if gp := os.Getenv("GOPATH"); gp != "" {
		candidates = append(candidates, filepath.Join(gp, "pkg", "mod"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "go", "pkg", "mod"))
	}
	if h := os.Getenv("HOME"); h != "" {
		candidates = append(candidates, filepath.Join(h, "go", "pkg", "mod"))
	}

	suffix := filepath.Join("github.com", "yanyiwu", "gojieba@v1.4.7", "deps", "cppjieba", "dict")
	for _, base := range candidates {
		if base == "" {
			continue
		}
		d := filepath.Join(base, suffix)
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
	}
	return ""
}

var stopWords = map[string]bool{
	// ── 代词 ──
	"我": true, "你": true, "他": true, "她": true, "它": true,
	"我们": true, "你们": true, "他们": true, "她们": true, "它们": true,
	"自己": true, "别人": true, "大家": true,
	"这": true, "那": true, "哪": true,
	"这个": true, "那个": true, "哪个": true,
	"这里": true, "那里": true, "哪里": true,
	"这些": true, "那些": true, "哪些": true,
	"什么": true, "怎么": true, "怎样": true, "怎么样": true,
	"谁": true, "为什么": true,
	// ── 量词 ──
	"一个": true, "每个": true,
	"种": true, "个": true, "些": true, "点": true,
	// ── 介词 ──
	"在": true, "到": true, "对": true, "从": true, "把": true,
	"被": true, "让": true, "给": true, "向": true, "往": true,
	"跟": true, "和": true, "与": true, "同": true, "比": true,
	"关于": true, "对于": true, "按照": true, "根据": true, "通过": true,
	"因为": true, "由于": true, "为了": true, "为": true,
	// ── 连词 ──
	"或": true, "或者": true,
	"但是": true, "但": true, "可是": true, "不过": true,
	"然而": true, "虽然": true, "尽管": true, "即使": true,
	"如果": true, "假如": true, "只要": true, "除非": true,
	"而且": true, "并且": true, "同时": true, "此外": true,
	"所以": true, "因此": true, "于是": true,
	"然后": true, "接着": true, "从而": true,
	"不是": true, "就是": true, "而是": true,
	// ── 助词 ──
	"的": true, "地": true, "得": true,
	"了": true, "着": true, "过": true,
	"所": true,
	"吗": true, "吧": true, "啊": true, "呢": true, "啦": true,
	"嗯": true, "哦": true, "哈": true, "呀": true, "嘛": true, "哟": true,
	"噢": true, "喔": true, "呵": true, "嘿": true, "喂": true,
	"呐": true, "呗": true, "咚": true, "噗": true,
	// ── 副词 ──
	"不": true, "没": true, "没有": true, "别": true, "不要": true,
	"很": true, "太": true, "非常": true, "十分": true, "特别": true,
	"比较": true, "相当": true, "更": true, "最": true,
	"都": true, "也": true, "还": true, "又": true, "再": true,
	"就": true, "才": true, "便": true,
	"已经": true, "曾经": true, "刚": true, "刚刚": true,
	"正在": true, "正": true,
	"将要": true, "将": true,
	"一直": true, "总是": true, "从来": true,
	"经常": true, "通常": true, "往往": true,
	"可能": true, "也许": true, "大概": true, "大约": true,
	"一定": true, "肯定": true, "必须": true,
	"当然": true, "其实": true, "确实": true,
	"一起": true, "一块": true,
	"仍然": true, "依然": true, "还是": true,
	"互相": true, "分别": true,
	"是否": true, "能否": true, "可否": true,
	"越": true, "挺": true,
	// ── 判断动词 ──
	"是": true, "有": true,
	"是的": true, "有的": true,
	// ── 能愿动词 ──
	"能": true, "能够": true, "可以": true, "会": true,
	"要": true, "需要": true, "想要": true,
	"应该": true, "应当": true, "该": true,
	"愿意": true, "肯": true, "敢": true,
	// ── 常用弱义动词 ──
	"说": true, "看": true, "做": true, "来": true, "去": true,
	"用": true, "想": true, "知道": true,
	"觉得": true, "认为": true, "发现": true, "看到": true,
	"表示": true, "告诉": true, "问": true, "回答": true,
	"成为": true, "作为": true, "进行": true, "使用": true,
	// ── 时间词（泛化） ──
	"今天": true, "昨天": true, "明天": true, "前天": true, "后天": true,
	"早上": true, "上午": true, "中午": true, "下午": true, "晚上": true,
	"现在": true, "目前": true, "之前": true, "之后": true, "以后": true,
	"最近": true, "刚才": true, "以前": true, "原来": true, "将来": true,
	// ── 人称/泛指 ──
	"人": true, "人们": true, "东西": true, "事情": true,
	"问题": true, "情况": true, "时候": true, "地方": true,
	"方式": true, "方法": true, "原因": true, "结果": true,
	// ── 人称代词补全（我们/你们/他们 早有，咱俩/咱们 漏了）──
	"咱俩": true, "咱们": true,
	// ── 限定/指代类：本身没有独立的指称对象 ──
	"任何": true, "此": true, "本": true, "其中": true, "以及": true,
	"那么": true, "这样": true, "那样": true, "一样": true,
	"还有": true, "还要": true, "只是": true, "老是": true,
	// ── 方位/整体类泛指 ──
	"全部": true, "所有": true, "有些": true, "一些": true,
	"别的": true, "其他": true, "其余": true, "各自": true, "本身": true,
	"上面": true, "下面": true, "里面": true, "外面": true,
	"前面": true, "后面": true, "左边": true, "右边": true,
	"中间": true, "附近": true, "周围": true, "部分": true, "方面": true,
	// ── 高频语气组合 ──
	"好的": true, "好吧": true, "好": true,
	"好了": true, "对了": true, "行了": true,
	"没事": true, "没关系": true, "算了": true,
	"看起来": true, "看上去": true, "听起来": true,
	"可以说": true, "也就是说": true, "的话": true,
	"来着": true, "罢了": true, "便是": true,
	// ── 对话/消息类弱义词 ──
	"消息": true, "回复": true, "发送": true,
	"查看": true, "显示": true, "输出": true, "输入": true,
	"文件": true, "内容": true, "信息": true,
	"来自": true, "收到": true,
	// ── 英文停用词 ──
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "could": true, "should": true,
	"may": true, "might": true, "can": true, "shall": true,
	"this": true, "that": true, "these": true, "those": true,
	"it": true, "its": true,
	"and": true, "or": true, "but": true, "in": true, "on": true,
	"at": true, "to": true, "for": true, "of": true, "with": true,
	"what": true, "how": true, "why": true, "which": true, "where": true,
	"when": true, "who": true, "whom": true,
	"please": true, "yes": true, "no": true, "not": true,
}

// TokenizeWords 使用 jieba 精确模式分词，返回去重后的所有词 token（不过滤停用词）
func TokenizeWords(text string) []string {
	text = CleanText(text)
	x := GetJieba()
	if x == nil {
		return nil
	}
	words := x.Cut(text, false)
	var result []string
	seen := make(map[string]bool)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		result = append(result, w)
	}
	return result
}

// TokenizeContentWords 使用 jieba 精确模式分词 + 词性过滤，只保留名词/动词/形容词/专名等有实义的词
// 过滤停用词 + 短词（<2 字符），返回去重结果。适用于向量化、关键词提取等需要语义质量的任务。
func TokenizeContentWords(text string) []string {
	text = CleanText(text)
	x := GetJieba()
	if x == nil {
		return nil
	}
	tagged := x.Tag(text)
	var result []string
	seen := make(map[string]bool)
	for _, t := range tagged {
		idx := strings.LastIndex(t, "/")
		if idx < 0 {
			continue
		}
		word := t[:idx]
		tag := t[idx+1:]
		word = strings.TrimSpace(word)
		if word == "" || seen[word] {
			continue
		}
		if stopWords[word] {
			continue
		}
		if utf8.RuneCountInString(word) < 2 {
			continue
		}
		if !contentPOS[tag] {
			continue
		}
		seen[word] = true
		result = append(result, word)
	}
	return result
}

func ExtractKeywords(text string) []string {
	return TokenizeContentWords(text)
}

// CutExact 精确模式分词：返回去停用词后的所有有义项（不限数量），用于 doc→graph 蒸馏
func CutExact(text string) []string {
	text = CleanText(text)
	x := GetJieba()
	if x == nil {
		return nil
	}
	words := x.Cut(text, false)
	var result []string
	seen := make(map[string]bool)
	for _, w := range words {
		if stopWords[w] || seen[w] {
			continue
		}
		if !validEntityName(w) {
			continue
		}
		seen[w] = true
		result = append(result, w)
	}
	return result
}
