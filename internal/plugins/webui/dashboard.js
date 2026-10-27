// ===== State =====
const state = {
	status: {},
	kernel: null,
	settings: {},
	meta: {},
	runtime: null, // /api/v1/runtime 的运行态小快照（调度器/驻留子/通道）
	pluginMeta: {},
	disabledPlugins: [],
	settingsPlugins: ["core"],
	selectedSection: "core",
	messages: [],
	chatLoading: false,
	chatStage: "",
	pipelinePhase: "", // 当前阶段（SSE stage 事件驱动总览页）
	pipelineTimer: null,
	stageTrail: [], // 本轮已发生的事件轨迹（工具/输出调用，供阶段管道展示循环）
	toolFlash: false, // 本轮新到一条工具调用：本轮渲染后给「工具」格放一次滑入动画
	healthResult: null,
	starmapInit: false,
	starmapLoading: false,
	starmapData: null,
	// ===== 星图：跟随 agent 活动 =====
	// 三路活动信号（均已在页面上，无需新数据源）：
	//   1. toolPulseMs —— SSE tool_call/stage 触发的「脉冲」，实时
	//   2. activity   —— /runtime 调度器计数（排队/中断/抢占），3s 轮询
	//   3. grownIds   —— /memory/graph/pulse 检出「新长出来」的节点，10s 轮询
	starmapPulses: [], // { mesh, until, kind } 活动脉冲队列
	starmapGrown: {}, // nodeId -> 生长动画截止时间戳
	starmapActivity: null, // /runtime 的调度器快照
	starmapPulseTimer: null, // /memory/graph/pulse 轮询 id
	starmapActivityTimer: null, // /runtime 轮询 id
	starmapLastPulseAt: 0, // 最后一次活动时间（ms），驱动全局呼吸
	starmapPulseSince: 0, // 下次 pulse 请求的回看起点（unix 秒）
	chatHistory: [],
	terminals: [],
	cmdHistory: [],
	termScreens: {},
	chatStick: true,
	pendingTools: [],
	eventSource: null,
	chatFinalIdx: -1,
	chatOffset: 0, // 分段历史：当前已加载消息在服务端全量中的起始下标
	chatTotal: 0, // 服务端历史总条数
	chatHasMore: false, // 是否还有更早历史可向上加载
	chatLastSeq: 0, // 增量轮询游标：已合并到本地的最大消息 seq
	lang: localStorage.getItem("ha-lang") || "zh",
};

// ===== I18n =====
window._i18n = {
	navOverview: ["概览", "Overview"],
	navChat: ["对话", "Chat"],
	navStarmap: ["星图", "Star Map"],
	navPlugins: ["插件", "Plugins"],
	navSettings: ["设置", "Settings"],
	navAdapters: ["适配器", "Adapters"],
	navKernel: ["内核", "Kernel"],
	navLogout: ["退出登录", "Logout"],
	themeToggle: ["切换亮色/暗色模式", "Toggle theme"],
};

function __(zh, en) {
	return state.lang === "en" ? en : zh;
}
function L() {
	return state.lang;
}

function toggleLang() {
	state.lang = state.lang === "zh" ? "en" : "zh";
	localStorage.setItem("ha-lang", state.lang);
	document.querySelectorAll("[data-i18n]").forEach((el) => {
		var k = el.getAttribute("data-i18n");
		var m = window._i18n && window._i18n[k];
		if (m) el.textContent = __(m[0], m[1]);
	});
	renderAll();
}

function applyI18n() {
	var lang = state.lang;
	var btn = document.getElementById("lang-btn");
	if (btn) btn.textContent = lang === "zh" ? "EN" : "中";
	document.querySelectorAll("[data-i18n]").forEach((el) => {
		var k = el.getAttribute("data-i18n");
		var m = window._i18n && window._i18n[k];
		if (m) el.textContent = lang === "en" ? m[1] : m[0];
	});
}

// ===== Theme =====
var ICON_SUN =
	'<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4m11.4-11.4 1.4-1.4"/></svg>';
var ICON_MOON =
	'<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>';

function setTheme(name) {
	document.documentElement.setAttribute("data-theme", name);
	var btn = document.getElementById("theme-btn");
	if (btn) btn.innerHTML = name === "light" ? ICON_SUN : ICON_MOON;
}

function saveTheme(name) {
	localStorage.setItem("ha-theme", name);
	setTheme(name);
}

function toggleTheme() {
	var cur = document.documentElement.getAttribute("data-theme");
	saveTheme(cur === "light" ? "dark" : "light");
}

var PALETTES = {
	// mono 放第一位：它现在是默认配色（黑白单调），也应当是选择器里最显眼那个。
	mono: "#c2c7d0",
	sakura: "#ff7fac",
	cyan: "#2dd4bf",
	violet: "#a78bfa",
	emerald: "#34d399",
	amber: "#fbbf24",
	blue: "#60a5fa",
};

function setColor(name) {
	document.documentElement.setAttribute("data-color", name);
	localStorage.setItem("ha-color", name);
	// 配色变了要强制重画运行态：拓扑的归属配色跟着 data-color 走，
	// 而数据签名没变，不置空 _rtSig 的话图会一直停在旧色。
	_rtSig = null;
	if (document.getElementById("rt-sec-topo")) {
		try {
			renderRuntime();
		} catch (e) {}
	}
	var pop = document.getElementById("palette-pop");
	if (!pop) return;
	var btns = pop.querySelectorAll("button.cdot");
	for (var i = 0; i < btns.length; i++) {
		btns[i].className =
			btns[i].getAttribute("data-c") === name ? "cdot on" : "cdot";
	}
}

function applyBgImg(url) {
	url = (url || "").trim();
	if (!url) {
		document.documentElement.style.setProperty("--bg-img", "none");
		localStorage.removeItem("ha-bg-img");
		return;
	}
	document.documentElement.style.setProperty(
		"--bg-img",
		'url("' + url.replace(/"/g, '\\"') + '")',
	);
	localStorage.setItem("ha-bg-img", url);
}

function applyBgBlur(n) {
	n = Math.max(0, Math.min(30, Number(n) || 0));
	document.documentElement.style.setProperty("--bg-blur", String(n));
	localStorage.setItem("ha-bg-blur", String(n));
	var v = document.getElementById("bg-blur-val");
	if (v) v.textContent = n + "px";
	var r = document.getElementById("bg-blur-range");
	if (r) r.value = String(n);
}

function toggleAppearance() {
	var pop = document.getElementById("palette-pop");
	if (!pop) return;
	var on = pop.classList.contains("on");
	if (!pop.querySelector("button.cdot")) {
		var cur = localStorage.getItem("ha-color") || "mono";
		var img = localStorage.getItem("ha-bg-img") || "";
		var blur = localStorage.getItem("ha-bg-blur") || "0";
		var dots = "";
		Object.keys(PALETTES).forEach((k) => {
			dots +=
				'<button class="cdot" data-c="' +
				k +
				'" title="' +
				k +
				'" style="background:' +
				PALETTES[k] +
				'"' +
				(k === cur ? "" : "") +
				" onclick=\"setColor('" +
				k +
				"')\"></button>";
		});
		pop.innerHTML =
			"<h4>主题色</h4><div>" +
			dots +
			"</div>" +
			'<h4 style="margin-top:8px">背景图片 URL</h4>' +
			'<input type="text" id="bg-img-input" placeholder="https://...jpg / png" value="' +
			escHtml(img) +
			'">' +
			'<div class="pp-row"><button class="btn btn-ghost btn-sm" onclick="applyBgImg(document.getElementById(\'bg-img-input\').value)">应用</button>' +
			'<button class="btn btn-ghost btn-sm" onclick="applyBgImg(\'\')">清除</button>' +
			'<span class="pp-val" style="margin-left:auto;min-width:90px;font-size:10px">模糊 ' +
			'<input type="range" id="bg-blur-range" min="0" max="30" value="' +
			blur +
			'" oninput="applyBgBlur(this.value)" style="width:80px;display:inline-block;vertical-align:middle">' +
			'<span id="bg-blur-val">' +
			blur +
			"px</span></span></div>";
		setColor(cur);
		var rr = document.getElementById("bg-blur-range");
		if (rr) rr.value = blur;
		var vv = document.getElementById("bg-blur-val");
		if (vv) vv.textContent = blur + "px";
	}
	pop.classList.toggle("on", !on);
}

(function initAppearance() {
	var img = localStorage.getItem("ha-bg-img");
	var blur = localStorage.getItem("ha-bg-blur");
	if (img) applyBgImg(img);
	if (blur) applyBgBlur(blur);
})();

(() => {
	var saved = localStorage.getItem("ha-theme");
	setTheme(saved || "light");
	// 默认配色 = 黑白：必须**主动** setColor 一次。只设 data-theme 时
	// base 主题仍是樱粉（:root 的 --accent 就是粉色），不写 data-color 就
	// 会默认花。用户选过别的颜色时以 localStorage 为准。
	setColor(localStorage.getItem("ha-color") || "mono");
	var sb = localStorage.getItem("ha-sidebar");
	if (sb === "1" || (!sb && window.innerWidth <= 768)) {
		document.body.classList.add("sidebar-collapsed");
	}
})();

// 字节数人性化显示（附件卡片用）
function formatBytes(n) {
	if (!n || n <= 0) return "";
	var units = ["B", "KB", "MB", "GB"];
	var i = 0;
	while (n >= 1024 && i < units.length - 1) {
		n /= 1024;
		i++;
	}
	return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
}

// ===== Utility =====
function renderMd(text) {
	if (typeof text !== "string") text = String(text || "");
	var html;
	if (typeof marked !== "undefined") {
		try {
			html = marked.parse(text);
		} catch (e) {
			html = escHtml(text);
		}
	} else {
		html = "<pre>" + escHtml(text) + "</pre>";
	}
	if (
		typeof DOMPurify !== "undefined" &&
		typeof DOMPurify.sanitize === "function"
	) {
		try {
			return DOMPurify.sanitize(html, { USE_PROFILES: { html: true } });
		} catch (e) {}
	}
	return html
		.replace(/<script[\s\S]*?<\/script>/gi, "")
		.replace(/\son\w+\s*=\s*"[^"]*"/gi, "")
		.replace(/\son\w+\s*=\s*'[^']*'/gi, "")
		.replace(/javascript:/gi, "");
}
function escHtml(s) {
	return String(s)
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;")
		.replace(/"/g, "&quot;");
}

function toast(m, isError) {
	var t = document.getElementById("toast");
	t.textContent = m;
	t.className = "toast" + (isError ? " error" : "");
	t.style.display = "block";
	clearTimeout(t._hideTimer);
	t._hideTimer = setTimeout(() => {
		t.style.display = "none";
	}, 3000);
}

// ===== 8.6 Unified toast + confirm dialog =====

// ===== 8.4 Card 3D tilt + cursor glow =====
document.addEventListener("mousemove", (e) => {
	var card = e.target.closest ? e.target.closest(".card.tilt") : null;
	if (card) {
		var r = card.getBoundingClientRect();
		card.style.setProperty("--mx", e.clientX - r.left + "px");
		card.style.setProperty("--my", e.clientY - r.top + "px");
		var rx = ((e.clientY - r.top) / r.height - 0.5) * -4;
		var ry = ((e.clientX - r.left) / r.width - 0.5) * 4;
		card.style.transform =
			"perspective(1000px) rotateX(" +
			rx.toFixed(2) +
			"deg) rotateY(" +
			ry.toFixed(2) +
			"deg) translateY(-1px)";
	}
});
document.addEventListener("mouseleave", (e) => {
	var card = e.target.closest ? e.target.closest(".card.tilt") : null;
	if (card) card.style.transform = "";
});

// ===== 首启人格向导 =====
// 人格是配置项（core.agent.personal_prompt，默认模板不含任何版本号）。
// 首次启动问一次「默认 / 自定义 / 稍后」，之后不再打扰；
// 不回答 = 稍后 = 保留默认人格，绝不阻塞启动。
async function maybeShowPersonaWizard() {
	var st;
	try {
		st = await api("/persona");
	} catch (e) {
		return; // 拿不到状态就不打扰用户
	}
	if (!st || st.initialized) return;
	var ov = document.createElement("div");
	ov.className = "confirm-overlay";
	ov.style.display = "flex";
	ov.innerHTML =
		'<div class="confirm-box">' +
		"<h3>" +
		escHtml(__("人格设定", "Persona")) +
		"</h3>" +
		"<p>" +
		escHtml(
			__(
				"首次启动：选一下助手的人格。选「使用默认」即可（之后可在设置里修改）；自定义内容在下次重启后生效。",
				'First run: pick your assistant\'s persona. "Use default" is fine (change it later in Settings); custom content takes effect after the next restart.',
			),
		) +
		"</p>" +
		'<div class="persona-warn"></div>' +
		'<textarea class="persona-ta" rows="6"></textarea>' +
		'<div class="persona-actions">' +
		'<button class="btn btn-ghost btn-sm" data-mode="later">' +
		escHtml(__("稍后再说", "Later")) +
		"</button>" +
		'<button class="btn btn-ghost btn-sm" data-mode="custom">' +
		escHtml(__("自定义…", "Custom…")) +
		"</button>" +
		'<button class="btn btn-sm" data-mode="default">' +
		escHtml(__("使用默认", "Use default")) +
		"</button>" +
		"</div></div>";
	document.body.appendChild(ov);
	var ta = ov.querySelector(".persona-ta");
	var warn = ov.querySelector(".persona-warn");
	var customOpen = false;
	if (st.file_override) {
		warn.style.display = "block";
		warn.textContent = __(
			"注意：检测到 personal/personal.md，它优先于这里的设置。",
			"Note: personal/personal.md exists and takes precedence over this choice.",
		);
	}
	function close() {
		ov.remove();
	}
	async function submit(mode, content) {
		try {
			var r = await api("/persona", {
				method: "POST",
				body: JSON.stringify({ mode: mode, content: content || "" }),
			});
			if (r && r.restart_required)
				toast(__("已保存，重启后生效", "Saved; takes effect after restart"));
			else toast(__("已保存", "Saved"));
		} catch (e) {
			toast(__("保存失败：", "Save failed: ") + e, true);
		}
		close();
	}
	ov.querySelectorAll("button[data-mode]").forEach((b) => {
		b.onclick = () => {
			var mode = b.getAttribute("data-mode");
			if (mode !== "custom") {
				submit(mode);
				return;
			}
			if (!customOpen) {
				// 第一次点：展开文本域并预填当前人格
				customOpen = true;
				ta.style.display = "block";
				ta.value = st.current_prompt || "";
				ta.focus();
				return;
			}
			if (!ta.value.trim()) {
				toast(__("内容不能为空", "Content cannot be empty"), true);
				return;
			}
			submit("custom", ta.value);
		};
	});
}

// ===== API =====
async function api(p, o) {
	var opts = {
		credentials: "include",
		headers: { "Content-Type": "application/json", ...o?.headers },
		...o,
	};
	// FormData 时不能手动设 Content-Type（浏览器需自动生成 multipart boundary）
	if (o?.body instanceof FormData) {
		delete opts.headers["Content-Type"];
	}
	var r = await fetch("/api/v1" + p, opts);
	if (r.status === 401) {
		location.href = "/login";
		throw new Error("unauthorized");
	}
	if (opts.raw) return r;
	var ct = r.headers.get("content-type") || "";
	if (ct.includes("json")) return r.json();
	return r.text();
}

// ===== Navigation =====
function switchTab(n) {
	document.querySelectorAll(".tab-content").forEach((e) => {
		e.classList.remove("active");
	});
	var el = document.getElementById("tab-" + n);
	if (el) el.classList.add("active");
	document.querySelectorAll("nav a").forEach((e) => {
		e.classList.remove("active");
	});
	var match = document.querySelector('nav a[onclick*="' + n + '"]');
	if (match) match.classList.add("active");
	var crumb = document.querySelector(".crumb-current");
	if (crumb) {
		var crumbKey = "nav" + n.charAt(0).toUpperCase() + n.slice(1);
		var m = window._i18n && window._i18n[crumbKey];
		crumb.setAttribute("data-i18n", crumbKey);
		if (m) crumb.textContent = __(m[0], m[1]);
		else crumb.textContent = n;
	}
	if (window.innerWidth <= 768) {
		document.body.classList.add("sidebar-collapsed");
	}
	if (n === "chat") {
		state.chatStick = true;
		var msgsEl = document.getElementById("chat-msgs");
		if (msgsEl) {
			msgsEl.scrollTop = msgsEl.scrollHeight;
		}
		initChatDragDrop();
	}
	if (n === "starmap") {
		// 独立星图页签：把 canvas 搬过来并按新容器尺寸重算。
		// 必须在 renderAll 之前做，否则 renderChatStarmap 会按上一次的
		// 活跃容器（总览页）算尺寸。
		var sc = document.getElementById("sm-container-page");
		if (sc) {
			if (starmapRen) {
				if (starmapRen.domElement.parentElement !== sc) {
					sc.appendChild(starmapRen.domElement);
					starmapRen.domElement.style.display = "block";
				}
				onStarmapResize();
			} else if (!state.starmapInit && !state.starmapLoading) {
				loadChatStarmapData();
			}
			// ★ 重新点火渲染循环。
			//
			// 循环在不可见时会自行 return（见 starmapAnimate，修「非常卡」），
			// 所以切回星图必须显式唤醒——否则图冻在切走前的那一帧。
			starmapWake();
		}
	}
	// 总览页与聊天面板也内嵌星图：回到它们时同样要唤醒。
	if (n === "overview" || n === "chat") starmapWake();
	renderAll();
}

function toggleSidebar() {
	document.body.classList.toggle("sidebar-collapsed");
	var saved = localStorage.getItem("ha-sidebar");
	localStorage.setItem(
		"ha-sidebar",
		document.body.classList.contains("sidebar-collapsed") ? "1" : "0",
	);
}

// ===== Tab Render Dispatch =====

// TABS 是「哪个页签需要哪些数据」的**唯一真相表**。
//
// 为什么要有这张表（实测依据，非估算）：
//
//	改前 renderAll 无论当前在哪个页签，都无条件拉 9 个接口并渲染全部
//	7 个页签。生产实例首屏 792,933 B 里，**约 230KB 花在用户看不见的
//	隐藏 DOM 上**：/api/v1/kernel（152KB，只有「内核」页要）与
//	/api/v1/settings（72KB，只有「设置」页要）——而 renderKernel() /
//	renderOneSettings() 是在**隐藏的 tab 容器**里构建 DOM 的。
//
//	依赖关系是**实测**出来的（逐个 render 函数 grep 它读的 state.*），
//	不是猜的：
//	  renderOverview  → 无（只读 status/runtime/dom）★ 总览最便宜
//	  renderKernel    → state.kernel
//	  renderOneSettings → state.settings / state.meta
//	  renderPlugins   → state.kernel / state.installedPlugins /
//	                   state.disabledPlugins / state.pluginInfo
//	  renderAdapters  → 无（自拉 /api/v1/adapters）
//	  renderChat      → 自建布局；星图/终端/命令是其子面板
//
//	于是「切到哪页才拉哪页的数据」成为结构性正确，而不是靠 if 串联。
var TABS = {
	// ★ overview 必须拉 kernel：总览的 8 个 KPI 里有 5 个读它
	//   （插件数 / 版本 / LLM / 记忆 / 文档 / 运行时），取值都写成
	//   `(k && k.plugins)` 这种安全形式——于是缺数据时不报错，
	//   **只显示「插件 0、记忆 —、文档 —」**，看起来像“服务坏了”。
	//
	//   我先前只 grep 了 renderOverview 直接读的 state.*，**漏了它间接
	//   调用的 updateOverview**，就把 kernel 从总览的数据块里删掉了。
	//   线上实测印证：state.kernel=false 而 status/runtime 有值。
	//
	//   教训：依赖分析必须覆盖**整个调用链**（render → 内部调用的 update*），
	//   只看入口函数的直接引用会漏。kernel 拉过一次后不再重拉
	//   （见 starmapFetchBlock 的节流），152KB 只在首屏付一次。
	overview: {
		fetch: ["status", "runtime", "kernel"],
		render: ["renderOverview"],
	},
	chat: {
		fetch: ["status", "proxyServices", "terminals", "cmdHistory"],
		render: ["renderChat"],
	},
	starmap: { fetch: [], render: ["renderStarmapTab"] },
	plugins: {
		fetch: ["status", "kernel", "plugins", "runtime"],
		render: ["renderPlugins"],
	},
	settings: { fetch: ["settings", "status"], render: ["renderOneSettings"] },
	adapters: { fetch: ["status"], render: ["renderAdapters"] },
	kernel: { fetch: ["status", "kernel", "runtime"], render: ["renderKernel"] },
};

// starmapActiveTab 返回当前激活的页签名。
function starmapActiveTab() {
	var el = document.querySelector(".tab-content.active");
	if (el && el.id) return el.id.replace(/^tab-/, "");
	return "overview";
}

// loadedOnce 记录每个数据块是否已拉过。刷新语义刻意区分：
//   - 首次进入某页签：拉
//   - 之后每 15s 的 renderAll：**不**重拉 kernel/settings（它们几乎不变，
//     却占每拍 232KB —— 这是「空闲 53MB/h」的主因）
//   - 切回页签：仍然不重拉（数据没理由变），只重渲染
//     真正需要新鲜的（runtime/status）单独走快通道，见 STALE_EVERY。
var _loadedOnce = {};
var _lastFetchAt = {};
// STALE_EVERY：这几块数据是活的（运行态/状态），每 3s 允许重拉一次。
var STALE_EVERY = 3000;

// starmapFetchBlock 按名字拉一块数据。幂等且带节流。
async function starmapFetchBlock(name) {
	var now = Date.now();
	var volatile_ = name === "status" || name === "runtime";
	if (_loadedOnce[name]) {
		if (!volatile_) return;
		if (now - (_lastFetchAt[name] || 0) < STALE_EVERY) return;
	}
	_lastFetchAt[name] = now;
	switch (name) {
		case "status":
			try {
				var s = await api("/status");
				state.status = s;
				state.startedAt = s.startedAt ? new Date(s.startedAt).getTime() : null;
			} catch (e) {}
			break;
		case "runtime":
			try {
				var rt = await api("/runtime");
				state.runtime = rt;
			} catch (e) {
				state.runtime = null;
			}
			// 运行态变了就顺手让星图重算一次（读同一个快照，不额外发请求）。
			if (typeof starmapApplyActivity === "function") starmapApplyActivity();
			break;
		case "kernel":
			try {
				state.kernel = await api("/kernel");
			} catch (e) {}
			// 初次进页就用 /kernel 的累计把徐标填上，不等第一条消息。
			// 口径说明：/kernel 的 usage 是**进程生命周期累计**，
			// 与 usage_session（本会话）不同 —— 所以显式传来源标记，
			// 免得两者混在一起看。
			if (state.kernel && state.kernel.usage) {
				renderChatUsage(
					state.kernel.usage,
					null,
					__("进程累计", "process total"),
				);
			}
			break;
		case "settings":
			try {
				var s2 = await api("/settings");
				state.settings = s2.settings || {};
				state.meta = s2.meta || {};
				state.settingsPlugins = s2.plugins || ["core"];
				state.pluginMeta = s2.plugin_meta || {};
				state.disabledPlugins = s2.disabled_plugins || [];
			} catch (e) {}
			break;
		case "plugins":
			try {
				state.installedPlugins = await api("/plugins");
			} catch (e) {}
			break;
		case "proxyServices":
			try {
				await loadProxyServices();
			} catch (e) {}
			break;
		case "terminals":
			try {
				await loadTerminals();
			} catch (e) {}
			break;
		case "cmdHistory":
			try {
				await loadCmdHistory();
			} catch (e) {}
			break;
	}
	_loadedOnce[name] = true;
}

async function renderAll(force) {
	var tab = starmapActiveTab();
	var spec = TABS[tab] || TABS.overview;
	// force：变更操作后（保存设置/启停插件）需要重新拉那块数据。
	if (force) _loadedOnce = {};
	// 1) 只拉当前页签需要的数据（并发的别串行等）
	//
	// ★ 必须逐项容错（2026-10-05）：之前这里是裸的 Promise.all，
	//   任一 fetch 块抛错 ⇒ 整体 reject ⇒ **后面的 spec.render 根本不执行**
	//   ⇒ 整页白屏。真实案例：starmapFetchBlock("kernel") 里调用了
	//   一个作用域不对的函数（ReferenceError），
	//   而 TABS 的 overview / plugins / kernel 三页都含 "kernel" ⇒ 三页全白。
	//
	//   下面对照：第 2 步的 spec.render 有 typeof 守卫、星图有 try/catch，
	//   唯独这一步没有。而它跑在 render **之前**，一旦炸后面全不执行。
	var needs = spec.fetch || [];
	await Promise.all(
		needs.map((n) => {
			// 单块失败只记日志，不影响同批其它块与后续渲染。
			return starmapFetchBlock(n).catch((e) => {
				console.error("starmapFetchBlock(" + n + ")", e);
			});
		}),
	);
	// 2) 只渲染当前页签（+ 星图在多处出现，单独处理）
	try {
		spec.render.forEach((fn) => {
			if (typeof window[fn] === "function") window[fn]();
		});
	} catch (e) {
		console.error("renderAll(" + tab + ")", e);
	}
	// 星图：总览页内嵌 + 独立页签两处都要确保已初始化。
	if (tab === "overview" || tab === "starmap" || tab === "chat") {
		try {
			renderChatStarmap();
		} catch (e) {
			console.error("renderChatStarmap", e);
		}
		if (tab === "starmap") {
			try {
				renderStarmapTab();
			} catch (e) {
				console.error("renderStarmapTab", e);
			}
		}
	}
	// 3) 聊天页的子面板（终端/运行中命令）只在聊天页重建
	if (tab === "chat") {
		try {
			renderTerminals();
			renderCmdHistory();
		} catch (e) {
			console.error("chat sub-panels", e);
		}
	}
	// 4) 卡片倾斜高光：只在指针设备上加
	try {
		if (!window.matchMedia("(hover: none)").matches) {
			document
				.querySelectorAll(
					"#tab-overview .card, #tab-plugins .card, #tab-kernel .card",
				)
				.forEach((c) => {
					if (!c.querySelector(".tilt-glow")) {
						var g = document.createElement("span");
						g.className = "tilt-glow";
						c.appendChild(g);
						c.classList.add("tilt");
					}
				});
		}
	} catch (e) {}
	applyI18n();
}

// fmtBuild 把构建身份压成一行：commit + 构建时间（日期部分）。
//
// 未注入 ldflags 的本地构建会得到 "unknown"，此时只显示 SDK 版本，
// 避免把 "unknown / unknown" 这种噪音摆在概览卡上。
function fmtBuild(b, s) {
	var parts = [];
	var commit = (b && b.commit) || (s && s.commit) || "";
	if (commit && commit !== "unknown") parts.push(commit);
	var bt = (b && b.build_time) || (s && s.build_time) || "";
	if (bt && bt !== "unknown") {
		// 2026-09-03T05:55:35Z → 2026-09-03
		parts.push(String(bt).split("T")[0]);
	}
	var sdkv = (b && b.sdk_compatible) || (s && s.sdk_version) || "";
	if (sdkv) parts.push("SDK " + sdkv);
	return parts.length ? parts.join(" · ") : "-";
}

function fmtUptime(ms) {
	var s = Math.floor(ms / 1000);
	if (s < 60) return s + "s";
	var m = Math.floor(s / 60);
	s = s % 60;
	if (m < 60) return m + "m " + s + "s";
	var h = Math.floor(m / 60);
	m = m % 60;
	return h + "h " + m + "m " + s + "s";
}

var uptimeTick = null;
function startUptimeTicker() {
	if (uptimeTick) clearInterval(uptimeTick);
	uptimeTick = setInterval(() => {
		var el = document.querySelector("#ov-uptime");
		if (el && state.startedAt) {
			var now = Date.now();
			el.textContent = fmtUptime(now - state.startedAt);
		} else if (!state.startedAt) {
			var el2 = document.querySelector("#ov-uptime");
			if (el2) el2.textContent = "-";
		}
	}, 1000);
}

// ===== Overview =====

// ===== 运行态面板：把内核的调度器/驻留子/通道画出来 =====
//
// 数据来自 /api/v1/runtime（内核 internal/sdk 暴露的 KernelStatus 子集），
// 每 3 秒刷一次——运行态要"实时"，而 /kernel 是 30KB 级的全量状态，不适合秒级轮询。
//
// 面板从上到下：四个数字块回答"现在忙不忙"；阶段管道滑块回答"这一轮走到哪"；
// 五条队列条形 + 中断栈回答"堵在哪一级/压了几层"；最后的分带拓扑回答
// "哪条输入喂给哪个 agent、哪个 agent 往哪条输出写、各自负载多高"。
// 四级语义直接照抄内核（internal/agent/core/scheduler.go 的 Level 定义），
// 别自己起名字——前端叫法一旦和内核不一致，看板就成了误导。
var RT_LEVELS = [
	{ lv: 4, name: "L4", desc: "内核独占", cls: "rt-lv-4" },
	{ lv: 3, name: "L3", desc: "交互", cls: "rt-lv-3" },
	{ lv: 2, name: "L2", desc: "消息", cls: "rt-lv-2" },
	{ lv: 1, name: "L1", desc: "后台", cls: "rt-lv-1" },
];
var RT_CAP_NAMES = {
	1: "text",
	2: "file",
	4: "image",
	8: "audio",
	16: "structured",
};

// _rtSig 缓存上一次渲染的数据签名。
//
// 为什么必须缓存：运行态每 3s 轮询一次，数据绝大多数时候是**没变**的；
// 无条件 `innerHTML =` 会把整块 DOM（含各级条的 transition）每 3s 重建一遍，
// 视觉效果就是“首页一闪一闪”。签名相同就一个字节也不动。
var _rtSig = null;

// morph：把容器的 DOM 就地「形变」成 html 描述的样子。
//
// 为什么不用 innerHTML = html：整块重建会把**没变的**节点也换掉 ——
// SVG 过渡从头播、图片重解码、展开态丢失、滚动锚点重置。运行态每几秒
// 刷一次，未变节点占绝大多数，那些闪烁就是这么来的。
//
// 做法：按「子节点位置 + nodeName」递归对账。同名元素复用同一个 DOM 节点，
// 只同步发生变化的属性与文本；只有标签真的不同才替换。元素身份不变 ⇒
// CSS 过渡继续、不闪。
function morph(container, html) {
	var tpl = document.createElement("div");
	tpl.innerHTML = html;
	morphChildren(container, tpl);
}
function morphChildren(oldParent, newParent) {
	var olds = Array.prototype.slice.call(oldParent.childNodes);
	var news = Array.prototype.slice.call(newParent.childNodes);
	var n = Math.max(olds.length, news.length);
	for (var i = 0; i < n; i++) {
		var o = olds[i],
			x = news[i];
		if (!o && x) {
			oldParent.appendChild(x);
			continue;
		}
		if (o && !x) {
			oldParent.removeChild(o);
			continue;
		}
		if (o.nodeType !== x.nodeType || o.nodeName !== x.nodeName) {
			oldParent.replaceChild(x, o);
			continue;
		}
		if (o.nodeType === 3 || o.nodeType === 8) {
			if (o.nodeValue !== x.nodeValue) o.nodeValue = x.nodeValue;
			continue;
		}
		morphAttrs(o, x);
		morphChildren(o, x);
	}
}
function morphAttrs(o, x) {
	var i, a;
	for (i = o.attributes.length - 1; i >= 0; i--) {
		a = o.attributes[i];
		if (!x.hasAttribute(a.name)) o.removeAttribute(a.name);
	}
	for (i = 0; i < x.attributes.length; i++) {
		a = x.attributes[i];
		if (o.getAttribute(a.name) !== a.value) o.setAttribute(a.name, a.value);
	}
}

// rtSlider 把“值 / 上限”画成轨道 + 滑块 + 读数（比纯文本数字直观）。
function rtSlider(value, max, label, display) {
	var pct = max > 0 ? Math.max(0, Math.min(100, (value / max) * 100)) : 0;
	return (
		'<span class="rt-slider"><span class="rt-slider-label">' +
		escHtml(label) +
		'</span><span class="rt-slider-track"><i style="width:' +
		pct +
		'%"></i><b style="left:' +
		pct +
		'%"></b></span>' +
		'<span class="rt-slider-val">' +
		escHtml(String(display === undefined ? value : display)) +
		"</span></span>"
	);
}

// rtMini 是行内迷你条（登记/抢占这类要并排两个的量）。
function rtMini(value, max, label) {
	var pct = max > 0 ? Math.max(0, Math.min(100, (value / max) * 100)) : 0;
	return (
		'<span class="rt-mini" title="' +
		escHtml(label) +
		" " +
		value +
		'"><span class="rt-mini-track"><i style="width:' +
		pct +
		'%"></i></span>' +
		value +
		"</span>"
	);
}

function rtTile(num, label, sub, pct, active, warn) {
	return (
		'<div class="rt-tile' +
		(active ? " rt-active" : "") +
		(warn ? " rt-warn" : "") +
		'"><div class="rt-num">' +
		num +
		'</div><div class="rt-label">' +
		label +
		"</div>" +
		(sub ? '<div class="rt-sub">' + sub + "</div>" : "") +
		'<div class="rt-bar"><i style="width:' +
		Math.max(0, Math.min(100, pct || 0)) +
		'%"></i></div></div>'
	);
}

function rtCapsHtml(caps) {
	var out = "";
	Object.keys(RT_CAP_NAMES).forEach((bit) => {
		if (caps & Number(bit)) {
			out += '<span class="rt-cap">' + RT_CAP_NAMES[bit] + "</span>";
		}
	});
	return out ? '<span class="rt-caps">' + out + "</span>" : "";
}

// 归属配色：根用青色，驻留子轮转其余颜色（同一张图里能一眼分清谁是谁）。
var RT_OWNER_COLORS = [
	"#88c0d0",
	"#b48ead",
	"#a3be8c",
	"#ebcb8b",
	"#d08770",
	"#8fbcbb",
];
// 黑白配色下，拓扑归属也用灰阶 —— 否则整页只剩这张图还是一片彩。
var RT_OWNER_COLORS_MONO = [
	"#e6e9ef",
	"#c2c7d0",
	"#9aa0ad",
	"#7b818d",
	"#5f6570",
	"#454a55",
];
function rtOwnerColors() {
	return document.documentElement.getAttribute("data-color") === "mono"
		? RT_OWNER_COLORS_MONO
		: RT_OWNER_COLORS;
}

// rtOwnerGroups 把 inputch 按**归属**分组（owner == 根 agent id 与 "" 归一为根）。
function rtOwnerGroups(inputs, residents, rootID) {
	var order = [];
	var map = {};
	inputs.forEach((c) => {
		var o = c.owner || "";
		if (o === rootID) o = "";
		if (!map[o]) {
			map[o] = [];
			order.push(o);
		}
		map[o].push(c);
	});
	// 驻留子即使一条 inputch 都没划到也要出现——否则「子存在但看不见」
	// 与「子不存在」无法区分。
	residents.forEach((r) => {
		var o = r.id || "";
		if (o && o !== rootID && !map[o]) {
			map[o] = [];
			order.push(o);
		}
	});
	order.sort((a, b) => {
		if (a === "") return -1;
		if (b === "") return 1;
		return a < b ? -1 : 1;
	});
	var ci = 0;
	var OW = rtOwnerColors();
	return order.map((o) => {
		var res = null;
		residents.forEach((r) => {
			if (r.id === o) res = r;
		});
		var list = map[o] || [];
		return {
			owner: o,
			child: o !== "",
			label:
				o === ""
					? __("根 agent / 内核默认", "root agent / kernel default")
					: __("驻留子 ", "resident ") + o,
			color: o === "" ? OW[0] : OW[1 + (ci++ % (OW.length - 1))],
			list: list,
			count: list.length,
			res: res,
		};
	});
}

// ---- 阶段管道滑块 ----
//
// 七阶段与内核 sdk.Stage 一一对应（顺序即执行顺序），由 SSE `stage` 事件驱动：
// 哪个阶段在执行，滑块就滑到哪一格。空闲时整条管道降透明度，不做假动画。
//
// 为什么放进总览：阶段管道回答"这一轮走到哪一步"，总览其余图形回答"积压了多少"，
// 两者合起来才是运行态。此前它只是对话页一个 10px 的角标，等于看不见。
// 阶段管道：**循环流程 + 本轮轨迹**，不是单向滑块。
//
// 一轮里工具调用会反复回到「行动后」再接下一个工具（post_action →
// before_toolcall → after_toolcall → post_action …），还可能中途多次
// 输出，所以线性滑块本身就是错的表述。这里画成
//   输入 → 行动 ⇄(工具) → 输出 → 结束
// （工具那格带循环标记），下面再用一排 chip 记下**本轮真实发生过什么**：
// 普通工具与 output_* 输出通道调用用不同配色区分开来。
// 页面上**任何位置都不用 emoji/符号字符充当图标** —— 一律内联 SVG，
// 24x24 viewBox + currentColor stroke，随主题与状态变色，不额外引资源。
var RT_ICO = {
	in: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M21 12H8"/><path d="M13 6l-6 6 6 6"/></svg>',
	act: '<svg class="rt-ico" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3.2"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5.5 5.5l2.1 2.1M16.4 16.4l2.1 2.1M18.5 5.5l-2.1 2.1M7.6 16.4l-2.1 2.1"/></svg>',
	tool: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M14.5 6.5a3.8 3.8 0 0 1 5 5L10 21l-5-5z"/><path d="M14.5 6.5 17.5 9.5"/></svg>',
	out: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M4 12h13"/><path d="M13 6l6 6-6 6"/></svg>',
	done: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M20 6 9 17l-5-5"/></svg>',
	loop: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M20.5 12a8.5 8.5 0 1 1-2.5-6"/><path d="M21 3.5V9h-5.5"/></svg>',
	bolt: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M13 2 4.5 13.5H11l-1 8.5L18.5 10H12z"/></svg>',
	caret:
		'<svg class="rt-ico rt-ico-sm" viewBox="0 0 24 24"><path d="M6 9l6 6 6-6"/></svg>',
};

var RT_PIPE_GROUPS = [
	{ zh: "输入", en: "in", ico: "in" },
	{ zh: "行动", en: "act", ico: "act" },
	{ zh: "工具", en: "tool", ico: "tool", loop: true },
	{ zh: "输出", en: "out", ico: "out" },
	{ zh: "结束", en: "done", ico: "done" },
];
function rtPhaseGroup(phase) {
	switch (phase) {
		case "on_input":
			return 0;
		case "pre_action":
		case "post_action":
			return 1;
		case "before_toolcall":
		case "after_toolcall":
			return 2;
		case "before_output":
			return 3;
		case "after_output":
			return 4;
	}
	return -1;
}
// rtShortTool 把 `qq_get_message` / `output_send__qq` 压成尾段短名。
function rtShortTool(name) {
	var n = String(name || "");
	var i = n.lastIndexOf("__");
	if (i >= 0) n = n.slice(i + 2);
	return n.length > 14 ? n.slice(0, 13) + "…" : n;
}
// rtSlots 画一组「车位」式格槽：槽位数量固定可见，被占用的点亮。
//
// 为什么不用进度条：队列为 0 时进度条宽度就是 0，整行只剩一串文字，
// 看上去就是「这块空着」。格槽在 0 时仍有形状，占用多少一眼可数，
// 也不用为了「看得见」而给 0 画一条假进度。
function rtSlots(depth, slots, extraCls) {
	var n = Math.max(5, Math.min(16, slots || 5));
	var d = depth || 0;
	var out = '<span class="rt-slots ' + (extraCls || "") + '">';
	for (var i = 0; i < n; i++)
		out += '<i class="' + (i < d ? "on" : "") + '"></i>';
	// 溢出计数必须留在 .rt-slots 内：格槽是 flex 行，多一个兄弟节点会被挤出去，
	// 多一个兄弟节点会被挤到下一行，整块队列就串了。
	if (d > n) out += '<b class="rt-slots-more">+' + (d - n) + "</b>";
	return out + "</span>";
}
// rtChipEl 画一枚事件 chip。工具与输出通道调用各有配色；名称包进
// .rt-chip-t，让窄框里由它做省略号截断（.rt-chip 本身是 flex 容器，
// text-overflow 对直接子文本不生效，会被硬切掉半截）。
function rtChipEl(t) {
	var kind = t.kind || "stage";
	var ico = kind === "output" ? RT_ICO.out : kind === "tool" ? RT_ICO.tool : "";
	return (
		'<span class="rt-chip rt-chip-' +
		kind +
		'" title="' +
		escHtml(t.label) +
		'">' +
		ico +
		'<b class="rt-chip-t">' +
		escHtml(t.short || t.label) +
		"</b>" +
		(t.n > 1 ? '<i class="rt-chip-n">x' + t.n + "</i>" : "") +
		"</span>"
	);
}
// rtPipelineHtml 画阶段管道：**五个等大的表框**，每框里是本阶段本轮发生的事。
//
// 为什么不是「小圆点 + 连接线 + 9px 小字」：那种画法在总览里几乎读不出
// 「现在走到哪一步」，尺寸也远小于旁边的 KPI 框。现在与中断队列、KPI 用同
// 一套视觉语言（等大框 + 大号加粗标题），并且事件直接落在它所属的框里，
// 所以「这一步发生了什么」不需要靠颜色或图例去猜。
function rtPipelineHtml(phase, trail) {
	var g = rtPhaseGroup(phase);
	var cells = RT_PIPE_GROUPS.map((s, i) => {
		var items = (trail || []).filter((t) => (t.g | 0) === i);
		// 「工具」格是**循环**格：一轮里可能调几十次工具/输出通道。把每一次都
		// 追加成 chip，这格会被撑成一长条，读者反而看不出「现在正在调什么」。
		// 所以它只保留**最新一条**，旧条在同一行视口里向上滚走
		// （rtFlashLatestTool 触发滑入），右侧另给本轮累计次数。
		var cls = "rt-pipe-events";
		var body;
		if (!items.length) {
			body =
				'<span class="rt-chip rt-chip-none">' + __("无", "none") + "</span>";
		} else if (s.loop) {
			cls += " rt-pipe-scroll";
			var total = 0;
			for (var k = 0; k < items.length; k++) total += items[k].n || 1;
			body =
				rtChipEl(items[items.length - 1]) +
				'<i class="rt-scroll-count" title="' +
				__("本轮工具调用累计次数", "tool calls this turn") +
				'">x' +
				total +
				"</i>";
		} else {
			body = items.map(rtChipEl).join("");
		}
		return (
			'<div class="rt-pipe-cell' +
			(i === g ? " active" : "") +
			'">' +
			'<div class="rt-pipe-head">' +
			RT_ICO[s.ico] +
			"<b>" +
			__(s.zh, s.en) +
			"</b>" +
			(s.loop
				? '<em class="rt-loop" title="' +
					__(
						"工具调用会回到行动后，可多次",
						"tool calls loop back; may repeat",
					) +
					'">' +
					RT_ICO.loop +
					"</em>"
				: "") +
			"</div>" +
			'<div class="' +
			cls +
			'">' +
			body +
			"</div>" +
			"</div>"
		);
	}).join("");
	return (
		'<div class="rt-section-title">' +
		__("阶段管道", "Stage pipeline") +
		(g < 0 ? "　" + __("（空闲）", "(idle)") : "") +
		"</div>" +
		'<div class="rt-pipe-row' +
		(g < 0 ? " rt-pipe-idle" : "") +
		'">' +
		cells +
		"</div>"
	);
}

// rtTrailPush 把一条「本轮发生过的事」落到它实际发生的阶段列里。
// 同一阶段重复出现同一条（如同一工具连调 3 次）只累加计数，不刷屏。
function rtTrailPush(g, kind, label, short) {
	if (!state.stageTrail) state.stageTrail = [];
	var arr = state.stageTrail;
	var last = arr.length ? arr[arr.length - 1] : null;
	if (last && last.g === g && last.kind === kind && last.short === short) {
		last.n = (last.n || 1) + 1;
		return;
	}
	arr.push({ g: g, kind: kind, label: label, short: short, n: 1 });
	if (arr.length > 24) arr.shift();
}

// rtFlashLatestTool 让「工具」格里最新那条做一次「向上滚入」。
//
// morph 是就地改文本：新工具到来时 chip 节点不会被替换，纯 CSS 的
// animation 因此不会自动重放。这里摘类 → 强制 reflow → 重加类，重置动画
// 时间轴。prefers-reduced-motion 由样式表统一压到 0.01ms，无需在此判断。
function rtFlashLatestTool() {
	var box = document.querySelector(".rt-pipe-events.rt-pipe-scroll");
	if (!box) return;
	var chip = box.querySelector(".rt-chip");
	if (!chip) return;
	chip.classList.remove("rt-chip-enter");
	void chip.offsetWidth; // 强制 reflow：少了这句，重加类不会重启动画
	chip.classList.add("rt-chip-enter");
}

// ---- per-agent 负载 ----
//
// 负载 = 该 agent **自己的**调度器积压折算成的百分比。
//
// 为什么按级别加权：四级中断里 L4 是内核独占、L3 是交互，堵在 L4 一条比堵在
// L1 五条更严重；中断栈深度意味着有现场被压着。权重是启发式的（"满载"没有
// 硬定义），因此函数做成饱和式 backlog/(backlog+K)：单调、上界 100、不越界。
// context_full 单独加分：子上下文满了以后每轮都要压缩，本身就是高负载。
function rtLoadPct(sc, ctxFull) {
	sc = sc || {};
	var q = sc.interrupt_queues || [0, 0, 0, 0, 0];
	var backlog =
		(sc.ready_queue_depth || 0) +
		(sc.pending_interrupts || 0) * 1.5 +
		(sc.suspend_stack || 0) * 2 +
		(q[1] || 0) * 1 +
		(q[2] || 0) * 1.2 +
		(q[3] || 0) * 1.5 +
		(q[4] || 0) * 2;
	var pct = 100 * (backlog / (backlog + 8));
	if (ctxFull) pct += 15;
	return Math.max(0, Math.min(100, Math.round(pct)));
}

// rtAgents 把「inputch 归属 + 驻留子 + 各自的调度器积压」整理成每个 agent 一行。
// 根 agent 的积压来自 rt.scheduler；驻留子的来自它自己的 ResidentStatus
// （见 internal/sdk/status.go 的 ReadyQueueDepth 等四项）。
function rtAgents(rt) {
	var rootID = rt.agent_id || "";
	var groups = rtOwnerGroups(
		rt.input_channels || [],
		rt.residents || [],
		rootID,
	);
	var outChans = (rt.channels || []).filter(
		(c) => c.direction === "out" || c.direction === "io",
	);
	return groups.map((g) => {
		var id = g.owner || rootID;
		var outputs = [];
		var seen = {};
		function add(name) {
			if (!name || seen[name]) return;
			seen[name] = 1;
			outputs.push(name);
		}
		if (g.child) {
			// 驻留子：优先用父显式授权的输出；没有授权登记时退回它自己 inputch 的
			// 默认回程（`output` 字段）。
			((g.res && g.res.allowed_outputs) || []).forEach(add);
			if (!outputs.length)
				g.list.forEach((c) => {
					add(c.output);
				});
		} else {
			// 根 agent 可以写任何输出通道；上限交给渲染侧截断。
			outChans.forEach((c) => {
				add(c.name);
			});
		}
		var load = g.child
			? rtLoadPct(
					{
						ready_queue_depth: g.res && g.res.ready_queue_depth,
						pending_interrupts: g.res && g.res.pending_interrupts,
						suspend_stack: g.res && g.res.suspend_stack,
						interrupt_queues: (g.res && g.res.interrupt_queues) || [],
					},
					g.res && g.res.context_full,
				)
			: rtLoadPct(rt.scheduler || {}, false);
		return {
			id: id,
			child: g.child,
			label: g.label,
			color: g.color,
			inputs: g.list,
			outputs: outputs,
			load: load,
			res: g.res || null,
		};
	});
}

// 连线路径表：通道名 → path d。光点动画靠它把「哪个通道」翻成一条曲线。
var _rtEdgeIn = {};
var _rtEdgeOut = {};
var RT_SVGNS = "http://www.w3.org/2000/svg";

// rtAgentTopology 画「通道 → agent → 通道」的分带拓扑。
//
// 每个 agent 一条横带：左边是归它的 inputch，中间是它自己（圆环 = 负载），
// 右边是它能写的 outputch。连线即路由；光点沿连线跑表示消息正在流动。
// 与旧版「归属框 → 单个内核盒」的差别：内核盒只有一个，看不出"这条输入到底
// 喂给了哪个子"，而子 agent 才是运行态里最该看清的东西。
// rtTopoWidth 用**实测容器宽**当 viewBox 宽：viewBox 宽与渲染宽一致时缩放才是
// 1:1。此前固定 640 而容器 ~920，浏览器按 "meet" 把内容顶在左上、右侧空出一大块，
// 观感就是"间距不对"。
function rtTopoWidth() {
	var host = document.getElementById("rt-sec-topo");
	var w = host ? host.clientWidth : 0;
	if (!w) {
		var panel = document.getElementById("rt-panel");
		w = panel ? panel.clientWidth : 0;
	}
	if (!w || w < 360) w = 900;
	return Math.round(w) - 4;
}

// rtClip 按估算宽度截断长通道名（SVG 没有 text-overflow，只能自己量）。
// 全角按 1em、其余按 0.56em 估宽：只求不越界，不求像素级精确。
function rtClip(text, maxPx, fontPx) {
	var s = String(text == null ? "" : text);
	var w = 0;
	var i = 0;
	for (; i < s.length; i++) {
		var cw = s.charCodeAt(i) > 0x2e80 ? fontPx : fontPx * 0.56;
		if (w + cw > maxPx) break;
		w += cw;
	}
	if (i >= s.length) return s;
	return s.slice(0, Math.max(1, i - 1)) + "…";
}

function rtAgentTopology(agents) {
	var W = rtTopoWidth();
	var ROW = 30,
		BOX_H = 20,
		GAP = 14,
		NODE_R = 20,
		MAX_OUT = 8;
	// 三列按比例分：输入 0~32%，节点居中在 44%，输出 56%~100%，两侧留白对称。
	var IN_X = 0,
		IN_W = Math.round(W * 0.32);
	var NX = Math.round(W * 0.44),
		OUT_X = Math.round(W * 0.56),
		OUT_W = W - Math.round(W * 0.56) - 2;
	var out = [];
	_rtEdgeIn = {};
	_rtEdgeOut = {};
	function esc(s) {
		return String(s == null ? "" : s).replace(/[<>&]/g, (m) =>
			m === "<" ? "&lt;" : m === ">" ? "&gt;" : "&amp;",
		);
	}
	function pathD(x1, y1, x2, y2) {
		var mx = (x1 + x2) / 2;
		return (
			"M" +
			x1 +
			" " +
			y1 +
			" C" +
			mx +
			" " +
			y1 +
			" " +
			mx +
			" " +
			y2 +
			" " +
			x2 +
			" " +
			y2
		);
	}
	var y = 8;
	if (!agents.length) {
		out.push(
			'<text class="tp-hint" x="8" y="24">' +
				__("暂无通道 / agent", "no channels / agents") +
				"</text>",
		);
		y = 44;
	}
	agents.forEach((a) => {
		var nin = a.inputs.length;
		var nout = Math.min(a.outputs.length, MAX_OUT);
		var rows = Math.max(nin, nout, 1);
		var truncated = a.outputs.length > MAX_OUT;
		// 内容高必须**容得下节点块**（圆环 + 下方标签一行），否则单行带里节点和它的
		// 名字会溢出到上/下一个带上（"间距不对"的一个来源）。
		var nodeBlock = NODE_R * 2 + (a.res && a.res.context_full ? 52 : 20);
		var contentH = Math.max(
			rows * ROW,
			nodeBlock,
			truncated ? nout * ROW + 18 : 0,
		);
		var bandH = contentH + GAP;
		var cy = y + contentH / 2;
		// 短的一列在带内**居中**：连线短而对称，不再出现"左边挤在上半、右边铺满"的错位感。
		var inTop = y + (contentH - nin * ROW) / 2;
		var outTop = y + (contentH - nout * ROW) / 2;
		out.push(
			'<rect x="0" y="' +
				(y - 3) +
				'" width="' +
				W +
				'" height="' +
				(bandH - 4) +
				'" rx="10" fill="' +
				a.color +
				'" fill-opacity="0.05"/>',
		);
		out.push(
			'<rect x="0" y="' +
				(y - 3) +
				'" width="3" height="' +
				(bandH - 4) +
				'" rx="1.5" fill="' +
				a.color +
				'" fill-opacity="0.7"/>',
		);

		// 输入通道 → agent
		a.inputs.forEach((c, i) => {
			var ry = inTop + i * ROW + ROW / 2;
			out.push(
				'<rect x="' +
					IN_X +
					'" y="' +
					(ry - BOX_H / 2) +
					'" width="' +
					IN_W +
					'" height="' +
					BOX_H +
					'" rx="7" fill="rgba(255,255,255,0.05)" stroke="' +
					a.color +
					'" stroke-opacity="0.35"/>',
			);
			out.push(
				'<text class="tp-name" x="' +
					(IN_X + 10) +
					'" y="' +
					(ry + 4) +
					'">' +
					esc(rtClip(c.name, IN_W - 44, 12)) +
					"<title>" +
					esc(c.name) +
					"</title></text>",
			);
			if (c.capacity) {
				out.push(
					'<text class="tp-sub" x="' +
						(IN_X + IN_W - 9) +
						'" y="' +
						(ry + 4) +
						'" text-anchor="end">' +
						esc(c.capacity) +
						"</text>",
				);
			}
			var d = pathD(IN_X + IN_W, ry, NX - NODE_R - 2, cy);
			_rtEdgeIn[c.name] = d;
			out.push(
				'<path class="tp-edge tp-edge-in" d="' +
					d +
					'" stroke="' +
					a.color +
					'"/>',
			);
		});

		// agent 节点：两圈 + 一段负载弧（stroke-dasharray 画进度）
		var C = 2 * Math.PI * (NODE_R - 3);
		out.push(
			'<circle cx="' +
				NX +
				'" cy="' +
				cy +
				'" r="' +
				NODE_R +
				'" fill="rgba(13,13,22,0.72)" stroke="' +
				a.color +
				'" stroke-opacity="0.55"/>',
		);
		out.push(
			'<circle cx="' +
				NX +
				'" cy="' +
				cy +
				'" r="' +
				(NODE_R - 3) +
				'" fill="none" stroke="rgba(255,255,255,0.10)" stroke-width="3.5"/>',
		);
		out.push(
			'<circle cx="' +
				NX +
				'" cy="' +
				cy +
				'" r="' +
				(NODE_R - 3) +
				'" fill="none" stroke="' +
				a.color +
				'" stroke-width="3.5" stroke-linecap="round" stroke-dasharray="' +
				((a.load / 100) * C).toFixed(1) +
				" " +
				C.toFixed(1) +
				'" transform="rotate(-90 ' +
				NX +
				" " +
				cy +
				')"/>',
		);
		out.push(
			'<text class="tp-val" x="' +
				NX +
				'" y="' +
				(cy + 4) +
				'" text-anchor="middle">' +
				a.load +
				"</text>",
		);
		out.push(
			'<text class="tp-lab" x="' +
				NX +
				'" y="' +
				(cy + NODE_R + 15) +
				'" text-anchor="middle" fill="' +
				a.color +
				'">' +
				esc(rtClip(a.child ? a.id : __("根 agent", "root"), 140, 11)) +
				"</text>",
		);
		if (a.res && a.res.context_full) {
			out.push(
				'<text class="tp-warn" x="' +
					NX +
					'" y="' +
					(cy + NODE_R + 30) +
					'" text-anchor="middle">' +
					__("上下文已满", "ctx full") +
					"</text>",
			);
		}

		// agent → 输出通道
		a.outputs.slice(0, MAX_OUT).forEach((name, i) => {
			var ry = outTop + i * ROW + ROW / 2;
			out.push(
				'<rect x="' +
					OUT_X +
					'" y="' +
					(ry - BOX_H / 2) +
					'" width="' +
					OUT_W +
					'" height="' +
					BOX_H +
					'" rx="7" fill="rgba(255,255,255,0.04)" stroke="rgba(255,255,255,0.12)"/>',
			);
			out.push(
				'<text class="tp-name" x="' +
					(OUT_X + 10) +
					'" y="' +
					(ry + 4) +
					'">' +
					esc(rtClip(name, OUT_W - 20, 12)) +
					"<title>" +
					esc(name) +
					"</title></text>",
			);
			var d = pathD(NX + NODE_R + 2, cy, OUT_X, ry);
			_rtEdgeOut[a.id + "\u0000" + name] = d;
			out.push(
				'<path class="tp-edge tp-edge-out" d="' +
					d +
					'" stroke="' +
					a.color +
					'"/>',
			);
		});
		if (a.outputs.length > MAX_OUT) {
			out.push(
				'<text class="tp-hint" x="' +
					OUT_X +
					'" y="' +
					(outTop + nout * ROW + 11) +
					'">+' +
					(a.outputs.length - MAX_OUT) +
					" " +
					__("更多", "more") +
					"</text>",
			);
		}
		y += bandH;
	});
	var H = Math.max(60, y);
	return (
		'<div class="rt-section-title">' +
		__("拓扑（圆环 = 负载）", "Topology (ring = load)") +
		"</div>" +
		'<div class="rt-svg-wrap"><svg id="rt-topo-svg" class="rt-svg rt-topo-svg" viewBox="0 0 ' +
		W +
		" " +
		H +
		'" preserveAspectRatio="xMinYMin meet" width="' +
		W +
		'" height="' +
		H +
		'">' +
		out.join("") +
		"</svg></div>"
	);
}

// rtSpark 让一个光点沿一条连线跑一趟。
//
// 用 SMIL <animateMotion> 而不是 CSS offset-path / rAF：它由浏览器自己按
// SVG 用户坐标跑，不需要前端维护动画循环，也没有 path() 在缩放 viewBox 下
// 的坐标换算问题。跑完把节点摘掉，避免 DOM 累积。
function rtSpark(pathD, color) {
	if (!pathD) return;
	var svg = document.getElementById("rt-topo-svg");
	if (!svg) return;
	var c = document.createElementNS(RT_SVGNS, "circle");
	c.setAttribute("r", "3.2");
	c.setAttribute("fill", color || "#ffffff");
	c.setAttribute("class", "rt-spark");
	var am = document.createElementNS(RT_SVGNS, "animateMotion");
	am.setAttribute("dur", "0.85s");
	am.setAttribute("path", pathD);
	am.setAttribute("fill", "freeze");
	am.setAttribute("begin", "0s");
	c.appendChild(am);
	svg.appendChild(c);
	setTimeout(() => {
		if (c.parentNode) c.parentNode.removeChild(c);
	}, 950);
}
// 输入：某条 inputch 来消息了（SSE channel_input）。
function rtSparkInput(source) {
	rtSpark(_rtEdgeIn[source], "#88c0d0");
}
// 输出：某个 agent 往某条 outputch 写了东西（SSE agent_output）。
function rtSparkOutput(agentID, channel) {
	if (!channel || !agentID) return;
	rtSpark(_rtEdgeOut[agentID + "\u0000" + channel], "#ff7fac");
}

function renderRuntime() {
	var el = document.getElementById("rt-panel");
	if (!el) return;
	var rt = state.runtime;
	if (!rt) {
		el.innerHTML =
			'<div class="rt-empty">' +
			__("运行态数据不可用", "runtime unavailable") +
			"</div>";
		_rtSig = null;
		return;
	}
	var sc = rt.scheduler || {};
	var residents = rt.residents || [];
	var channels = rt.channels || [];
	var inputs = rt.input_channels || [];

	// 数据签名（**不含 uptime**——它每秒都变，带上就等于没缓存）：整体没变就整块跳过。
	// 签名里必须带 pipelinePhase：否则 SSE 把阶段推到下一格时，
	// 数据没变 → 早退 → 滑块不动，只能等下一次 /runtime 轮询才追上。
	var sig = JSON.stringify([
		sc,
		residents,
		channels,
		inputs,
		state.pipelinePhase,
		(state.stageTrail || [])
			.map(
				(t) =>
					t.g + ":" + t.kind + ":" + (t.short || t.label) + "x" + (t.n || 1),
			)
			.join(","),
	]);
	if (sig === _rtSig) return;
	_rtSig = sig;

	// 外壳只建一次；随后**逐段**更新。
	//
	// 为什么不是整块 innerHTML：设备通道列表这类数据本来就会来回变（实测远程设备
	// 通道 11→9 条），整块重建会把没变的段落（含条形 transition）也推倒重来，
	// 视觉上就是「一闪一闪」。逐段比较后只替换真正变了的那一段。
	if (!document.getElementById("rt-sec-tiles")) {
		el.innerHTML =
			'<div class="card"><h2>' +
			__("运行态", "Runtime") +
			"</h2>" +
			'<div id="rt-sec-tiles"></div>' +
			'<div id="rt-sec-pipe"></div>' +
			'<div id="rt-sec-levels"></div>' +
			'<div id="rt-sec-stack"></div>' +
			'<div id="rt-sec-topo"></div>' +
			"</div>";
	}
	function put(sec, html) {
		var n = document.getElementById("rt-sec-" + sec);
		if (!n || n.__sig === html) return; // 该段没变：一个字节都不动
		n.__sig = html;
		// 变了也只「形变」到新样子：复用未变的子节点，而不是整块重建。
		morph(n, html);
	}

	var q = sc.interrupt_queues || [0, 0, 0, 0, 0];
	var pending = sc.pending_interrupts || 0;
	var ready = sc.ready_queue_depth || 0;
	var stack = sc.suspend_stack || 0;
	var maxStack = sc.max_suspend_depth || 4;
	var frames = sc.suspend_frames || [];
	var byLv = sc.interrupts_by_level || [];
	var preLv = sc.preempts_by_level || [];

	// ---- 段 1：四个数字块 ----
	var h = '<div class="rt-grid">';
	h += rtTile(
		ready,
		__("排队", "Ready"),
		"",
		ready ? Math.min(100, ready * 20) : 0,
		ready > 0,
	);
	h += rtTile(
		pending,
		__("中断", "Pending"),
		"",
		pending ? Math.min(100, pending * 25) : 0,
		pending > 0,
		pending > 0,
	);
	h += rtTile(
		stack + "/" + maxStack,
		__("栈", "Stack"),
		"",
		(stack / (maxStack || 4)) * 100,
		stack > 0,
	);
	var rFull = residents.filter((r) => r.context_full).length;
	h += rtTile(
		residents.length,
		__("子代理", "Subagents"),
		rFull ? rFull + __("满", " full") : "",
		residents.length ? Math.min(100, residents.length * 20) : 0,
		residents.length > 0,
		rFull > 0,
	);
	h += "</div>";
	// 「累计：入队 … 执行 … 抢占 …」那一整行纯文字被去掉了：它对"现在忙不忙"
	// 没有帮助，却占掉一整行，是总览里最大的一坨文字。
	put("tiles", h);

	// ---- 段 2：阶段管道（滑块，SSE stage 事件驱动）----
	put("pipe", rtPipelineHtml(state.pipelinePhase, state.stageTrail));

	// ---- 段 3：队列（四级中断 + 一条排队）----
	//
	// 设计是「四条中断队列（L1–L4）+ 一条排队队列」共五个，所以必须画五行：
	// 只画四条会让「排队输入」这条线在运行态里凭空消失，而它正是
	// 「不需要及时处理」的那一半输入。排队队列**无级别**，故用不同配色 + 虚线。
	h = '<div class="rt-section-title">' + __("队列", "Queues") + "</div>";
	var maxQ = Math.max(1, ready, q[1] || 0, q[2] || 0, q[3] || 0, q[4] || 0);
	var maxReg = 1;
	var maxPre = 1;
	RT_LEVELS.forEach((L) => {
		maxReg = Math.max(maxReg, byLv[L.lv] || 0);
		maxPre = Math.max(maxPre, preLv[L.lv] || 0);
	});
	// 槽位数按全场最大深度缩放（且至少 5 格）：0 时也有可见形状，不空着。
	var qSlots = Math.max(5, Math.min(16, maxQ));
	// 五个**等大表框**（四级中断 + 一条排队），与阶段管道同一套视觉语言。
	// 此前是五行扁条，四级中断全为 0 时四行几乎全是空白，又占高度又难看。
	h += '<div class="rt-queues">';
	RT_LEVELS.forEach((L) => {
		var depth = q[L.lv] || 0;
		var reg = byLv[L.lv] || 0;
		var pre = preLv[L.lv] || 0;
		h +=
			'<div class="rt-qcell ' +
			L.cls +
			(depth ? " rt-active" : "") +
			'" title="' +
			escHtml(L.desc) +
			'">' +
			'<div class="rt-qhead"><b>' +
			L.name +
			"</b><span>" +
			escHtml(L.desc) +
			"</span></div>" +
			'<div class="rt-qnum">' +
			depth +
			"</div>" +
			rtSlots(depth, qSlots, L.cls) +
			'<div class="rt-qmeta">' +
			rtMini(reg, maxReg, __("登记", "registered")) +
			rtMini(pre, maxPre, __("抢占", "preempted")) +
			"</div></div>";
	});
	// 第五条：排队队列（无级别，纯 FIFO）。
	// 它不是优先级而是另一**类别**（排队 vs 中断），所以用虚线框区分。
	h +=
		'<div class="rt-qcell rt-qcell-queued rt-lv-q' +
		(ready ? " rt-active" : "") +
		'" title="' +
		__("排队（无级别，纯 FIFO）", "queued (no priority, FIFO)") +
		'">' +
		'<div class="rt-qhead"><b>' +
		__("排队", "queued") +
		"</b><span>FIFO</span></div>" +
		'<div class="rt-qnum">' +
		ready +
		"</div>" +
		rtSlots(ready, qSlots, "rt-lv-q") +
		'<div class="rt-qmeta">' +
		__("无级别", "no priority") +
		"</div></div>";
	h += "</div>";
	if (sc.immediate) {
		h +=
			'<div class="rt-frame">' +
			RT_ICO.bolt +
			" " +
			__("立即运行", "immediate") +
			"：" +
			escHtml(sc.immediate.kind || "") +
			" #" +
			sc.immediate.id +
			'<span class="rt-frame-top">L' +
			(sc.immediate.level || 0) +
			"</span></div>";
	}
	put("levels", h);

	// ---- 段 4：中断栈 ----
	h = '<div class="rt-section-title">' + __("栈", "Stack") + "</div>";
	// 深度先给一条进度条（"压了几层 / 上限几层"一眼可读），下面再列具体帧。
	h += rtSlider(stack, maxStack, __("深度", "depth"), stack + " / " + maxStack);
	if (frames.length) {
		h += '<div class="rt-stack">';
		frames.forEach((f, i) => {
			var t = (f && f.task) || {};
			h +=
				'<div class="rt-frame">' +
				escHtml(t.kind || "") +
				" #" +
				(t.id || "?") +
				'<span class="rt-frame-top">L' +
				(t.level || 0) +
				(i === frames.length - 1 ? " · " + __("栈顶", "top") : "") +
				"</span></div>";
		});
		h += "</div>";
	} else {
		h +=
			'<div class="rt-empty">' +
			__(
				"中断栈为空（当前无被抢占的现场）",
				"stack empty (nothing preempted)",
			) +
			"</div>";
	}
	if (sc.running) {
		h +=
			'<div class="rt-empty">' +
			__("正在运行", "running") +
			"：" +
			escHtml(sc.running.kind || "") +
			" #" +
			sc.running.id +
			" (L" +
			(sc.running.level || 0) +
			")</div>";
	}
	put("stack", h);

	// ---- 段 5：Agent 拓扑 ----
	//
	// 每个 agent 一条横带：左是归它的 inputch、中间是它自己（圆环 = 负载）、
	// 右是它能写的 outputch，连线即路由。归属、路由、容量、负载全部画进
	// 同一张 SVG，不再单开"通道分配"一节——那本来就是这个拓扑的一部分。
	put("topo", rtAgentTopology(rtAgents(rt)));
}

// loadRuntime 拉运行态小快照并就地重绘面板（约 2KB，可秒级轮询）。
async function loadRuntime() {
	try {
		var rt = await api("/runtime");
		state.runtime = rt;
		renderRuntime();
	} catch (e) {
		state.runtime = null;
	}
}

// startRuntimeTicker 起 3s 轮询：运行态只在总览页可见时才有意义，
// 其它页签上不浪费请求。切回总览时 renderAll 会立刻再拉一次，不必等下一拍。
// startChatTicker：聊天页可见时 3s 轮询一次增量（与运行态同一节奏）。
// SSE 仍在（token 级流式靠它），轮询是「数据查询 api」那条腿：
// 界面最终状态由轮询拉到的数据决定，SSE 只负责让流式看起来即时。
function startChatTicker() {
	if (state._chatTicker) return;
	state._chatTicker = setInterval(() => {
		var tab = document.querySelector("#tab-chat");
		if (tab && tab.classList.contains("active")) {
			syncChatFromHistory().catch(() => {});
		}
	}, 3000);
}

function startRuntimeTicker() {
	if (state._runtimeTicker) return;
	state._runtimeTicker = setInterval(() => {
		var tab = document.querySelector("#tab-overview");
		if (tab && tab.classList.contains("active")) {
			// 走共享数据块（自带 3s 节流 + 单一数据源）而不是直接 loadRuntime：
			// 星图也读 state.runtime，两个入口拉同一接口就是重复请求。
			starmapFetchBlock("runtime").then(() => {
				if (starmapActiveTab() === "overview") renderRuntime();
			});
		}
	}, 3000);
}

// ===== Overview =====
//
// 总览页是**静态骨架 + 数据绑定**：DOM 只在首帧建一次，之后所有刷新只改
// 文本/类名，绝不重建节点。此前 renderAll 每 15s 把整个总览（含运行态面板）
// innerHTML 重建一遍 —— 那是页面上最刺眼的周期性闪烁的来源。
//
// 文字也压到最少：状态用彩色圆点表达，其余只留数字与两字标签，不再堆
// 「未初始化 / 不可用」这类整句描述。
var OV_ICONS = {
	activity:
		'<svg viewBox="0 0 24 24"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>',
	clock:
		'<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>',
	puzzle:
		'<svg viewBox="0 0 24 24"><path d="M4 7h4a2 2 0 1 1 4 0h4v4a2 2 0 1 0 0 4v4h-4a2 2 0 1 0-4 0H4v-4a2 2 0 1 1 0-4z"/></svg>',
	tag: '<svg viewBox="0 0 24 24"><path d="M20 12l-8 8-9-9V3h8z"/><circle cx="7.5" cy="7.5" r="1.3"/></svg>',
	brain:
		'<svg viewBox="0 0 24 24"><path d="M9 3a3 3 0 0 0-3 3 3 3 0 0 0-1 5.8V15a3 3 0 0 0 4 2.8V21"/><path d="M15 3a3 3 0 0 1 3 3 3 3 0 0 1 1 5.8V15a3 3 0 0 1-4 2.8V21"/></svg>',
	db: '<svg viewBox="0 0 24 24"><ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/></svg>',
	file: '<svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>',
	cpu: '<svg viewBox="0 0 24 24"><rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/></svg>',
};

function ovKpi(id, icon, label, dot) {
	return (
		'<div class="ov-kpi"><span class="ov-ico">' +
		icon +
		'</span><span class="ov-val">' +
		(dot ? '<i class="ov-dot" id="ov-' + id + '-dot"></i>' : "") +
		'<b id="ov-' +
		id +
		'">—</b></span>' +
		'<span class="ov-sub" id="ov-' +
		id +
		'-sub"></span>' +
		'<span class="ov-lab">' +
		label +
		"</span></div>"
	);
}

function renderOverview() {
	var host = document.getElementById("tab-overview");
	if (!host) return;
	// 只在首帧建骨架；之后（renderAll 每 15s 一次）只 updateOverview。
	// 一个节点都不重建，所以不会闪。
	if (!document.getElementById("ov-kpis")) {
		host.innerHTML =
			'<div id="rt-panel"></div>' +
			'<div class="card"><div class="ov-kpis" id="ov-kpis">' +
			ovKpi("status", OV_ICONS.activity, __("状态", "Status"), true) +
			ovKpi("uptime", OV_ICONS.clock, __("运行", "Uptime")) +
			ovKpi("plugins", OV_ICONS.puzzle, __("插件", "Plugins")) +
			ovKpi("version", OV_ICONS.tag, __("版本", "Version")) +
			ovKpi("llm", OV_ICONS.brain, "LLM", true) +
			ovKpi("memory", OV_ICONS.db, __("记忆", "Memory")) +
			ovKpi("docs", OV_ICONS.file, __("文档", "Docs")) +
			ovKpi("runtime", OV_ICONS.cpu, __("运行时", "Runtime")) +
			"</div></div>" +
			// 星图搬到主页：作为总览的门面，跟随 agent 活动脉动。
			// 开源许可单独成框：之前在 KPI 卡底部只是一行小链接（.ov-foot），
			// 几乎看不见，也看不出是许可证还是别的什么。
			'<div class="card"><h2>' +
			__("记忆星图", "Memory Star Map") +
			' <span class="badge" id="sm-home-badge" style="font-size:10px;font-weight:400"></span></h2>' +
			'<div id="sm-container-home" style="height:360px"></div></div>' +
			'<div class="card" id="ov-legal"></div>';
	}
	updateOverview();
	// 星图在主页常驻：容器已建好，首次就拉数据 + 初始化 3D。
	if (document.getElementById("sm-container-home") && !state.starmapInit) {
		renderChatStarmap();
	}
}

function ovSet(id, text) {
	var el = document.getElementById(id);
	if (el && el.textContent !== String(text)) el.textContent = String(text);
}
function ovDot(id, cls) {
	var el = document.getElementById(id);
	if (el && el.className !== cls) el.className = cls;
}

// updateOverview 只写值与类名，从不改结构。
function updateOverview() {
	var s = state.status || {};
	var k = state.kernel;
	var running = s.status === "running";
	ovSet("ov-status", running ? __("运行中", "running") : s.status || "unknown");
	ovDot("ov-status-dot", "ov-dot " + (running ? "ok" : "warn"));
	ovSet(
		"ov-uptime",
		state.startedAt ? fmtUptime(Date.now() - state.startedAt) : "-",
	);
	ovSet("ov-plugins", ((k && k.plugins) || []).length || 0);
	// 内核身份：光有版本号分不清是哪个内核、哪次构建 —— 补上内核名与 commit。
	// 之前 36b577b 改图标 KPI 时把 kernel_name 丢了，只剩余 "v1.4.0"。
	var kb = (k && k.build) || {};
	var kv = kb.version || s.version || "";
	ovSet("ov-version", kv ? "v" + kv : "—");
	ovSet(
		"ov-version-sub",
		kv
			? (kb.kernel_name || "HologramAgent") +
					(kb.commit && kb.commit !== "unknown"
						? " · " + String(kb.commit).slice(0, 7)
						: "")
			: "",
	);
	var llm = (k && k.llm) || {};
	ovDot("ov-llm-dot", "ov-dot " + (llm.available ? "ok" : "bad"));
	ovSet("ov-llm", llm.provider || "—");
	var mem = (k && k.memory) || {};
	ovSet(
		"ov-memory",
		mem.available ? mem.entity_count + "/" + mem.relation_count : "—",
	);
	var docs = (k && k.documents) || {};
	ovSet("ov-docs", docs.available ? docs.doc_count : "—");
	var rt = (k && k.runtime) || {};
	ovSet(
		"ov-runtime",
		rt.goroutines
			? rt.goroutines + (rt.memory_mb ? " · " + rt.memory_mb + "M" : "")
			: "—",
	);
	// 开源许可卡：协议标识 + 协议全文 + 源码仓库。
	//
	// 为什么单独成框、且三者都给：AGPL-3.0 §13 的义务是「向网络使用者提供
	// 取得 Corresponding Source 的机会」——只给一个仓库链接、不写协议名，
	// 使用者看不出这受什么许可约束，也看不出网络服务场景下还有 §13 的义务。
	// 内容对一次构建是常量，所以填一次就够（__html 比对避免重复重建）。
	var legal = document.getElementById("ov-legal");
	if (legal) {
		var lb = (k && k.build) || {};
		var src = lb.source_url || "";
		var lic = lb.license || "";
		var licURL = lb.license_url || "";
		var rows = "";
		if (lic) {
			rows +=
				'<div class="kv-row"><span class="key">' +
				__("许可协议", "License") +
				'</span><span class="val">' +
				(licURL
					? '<a href="' +
						escHtml(licURL) +
						'" target="_blank" rel="noopener noreferrer">' +
						escHtml(lic) +
						"</a>"
					: escHtml(lic)) +
				"</span></div>";
		}
		if (src) {
			rows +=
				'<div class="kv-row"><span class="key">' +
				__("源码仓库", "Source") +
				'</span><span class="val"><a href="' +
				escHtml(src) +
				'" target="_blank" rel="noopener noreferrer">' +
				escHtml(src) +
				"</a></span></div>";
		}
		// 网络条款只在 AGPL 系的许可下才成立，所以按标识判断，不硬写协议名。
		var note =
			lic && lic.toUpperCase().indexOf("AGPL") >= 0
				? '<p class="ov-legal-note">' +
					__(
						"网络服务条款（§13）：把修改后的版本作为网络服务对外提供时，必须向使用者提供取得对应源码的途径。",
						"Network clause (section 13): offering a modified version as a network service requires giving users a way to obtain the Corresponding Source.",
					) +
					"</p>"
				: "";
		var want = rows
			? "<h2>" + __("开源许可", "License") + "</h2>" + rows + note
			: "";
		if (legal.__html !== want) {
			legal.__html = want;
			legal.innerHTML = want;
		}
	}
}

// ===== Chat =====
var _chatLayoutBuilt = false;

function buildChatLayout() {
	var cont = document.getElementById("tab-chat");
	var k = state.kernel || {};
	var html = '<div class="chat-layout">';
	html +=
		'<div class="chat-tabs">' +
		'<span class="active" onclick="switchChatPanel(\'chat\',this)">' +
		__("对话", "Chat") +
		"</span>" +
		"<span onclick=\"switchChatPanel('starmap',this)\">" +
		__("星图", "Star Map") +
		"</span>" +
		"<span onclick=\"switchChatPanel('terminal',this)\">" +
		__("终端", "Terminal") +
		"</span>" +
		"<span onclick=\"switchChatPanel('cmd',this)\">" +
		__("运行中命令", "Running Commands") +
		"</span>" +
		"<span onclick=\"switchChatPanel('memory',this)\">" +
		__("记忆", "Memory") +
		"</span>" +
		"<span onclick=\"switchChatPanel('context',this)\">" +
		__("上下文", "Context") +
		"</span>" +
		"<span onclick=\"switchChatPanel('knowledge',this)\">" +
		__("知识", "Knowledge") +
		"</span></div>";
	html +=
		'<div class="chat-panel active" id="chat-panel-chat"><div class="chat-main">';
	html +=
		'<div class="card"><h2>' +
		__("对话", "Chat") +
		' <span id="chat-stage" class="badge" style="font-size:10px;font-weight:400;display:none">' +
		escHtml(state.chatStage || "") +
		"</span>" +
		// 用量徐标：本会话累计 token 与缓存命中率。
		// 内核一直在推 agent_llm_chain（带 usage/usage_session），
		// 前端此前从未订阅 ⇒ 「发了但没人收到」，页面上看不到成本。
		' <span id="chat-usage" class="badge" style="font-size:10px;font-weight:400;display:none;cursor:help"></span>' +
		'</h2><div class="chat-messages" id="chat-msgs">';
	if (state.messages.length === 0) {
		html +=
			'<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' +
			__(
				"开始对话以测试 Agent 回复",
				"Start a conversation to test Agent replies",
			) +
			"</p></div>";
	}
	html +=
		"</div>" +
		'<div class="chat-input-row">' +
		'<input type="file" id="chat-file" style="display:none" onchange="sendChatFile(this.files[0])">' +
		'<button class="btn" onclick=\'document.getElementById("chat-file").click()\' id="chat-file-btn" title="' +
		__("发送文件", "Send file") +
		'" style="padding:0 12px;display:flex;align-items:center">' +
		'<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21.44 11.05l-9.19 9.19a6 6 0 01-8.49-8.49l9.19-9.19a4 4 0 015.66 5.66l-9.2 9.19a2 2 0 01-2.83-2.83l8.49-8.48"/></svg>' +
		"</button>" +
		'<input id="chat-input" placeholder="' +
		__("输入消息...", "Type a message...") +
		'" onkeydown="if(event.key==\'Enter\')sendChat()">' +
		'<button class="btn" onclick="interruptChat()" id="chat-stop-btn" style="display:none;background:var(--danger, #d1383d);color:#fff">' +
		__("停止", "Stop") +
		"</button>" +
		'<button class="btn btn-primary" onclick="sendChat()" id="chat-send-btn">' +
		__("发送", "Send") +
		"</button>" +
		"</div></div></div></div>";
	html +=
		'<div class="chat-panel" id="chat-panel-starmap"><div class="card"><h2>' +
		__("星图", "Star Map") +
		"</h2>" +
		'<div id="sm-container-chat" style="display:flex;align-items:center;justify-content:center;min-height:480px"><div class="loading-spinner"></div></div></div></div>';
	html +=
		'<div class="chat-panel" id="chat-panel-terminal"><div class="card"><h2>' +
		__("终端", "Terminal") +
		' <span id="term-count-badge" class="badge badge-blue">0</span></h2>' +
		'<div id="term-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
	html +=
		'<div class="chat-panel" id="chat-panel-cmd"><div class="card"><h2>' +
		__("命令历史", "Command History") +
		' <span id="cmd-count-badge" class="badge badge-blue">0</span></h2>' +
		'<div id="cmd-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
	html +=
		'<div class="chat-panel" id="chat-panel-memory"><div class="card"><h2>' +
		__("记忆", "Memory") +
		"</h2>" +
		'<div class="kv-row"><span class="key">' +
		__("实体", "Entities") +
		'</span><span class="val">' +
		(k?.memory?.entity_count || "-") +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("关系", "Relations") +
		'</span><span class="val">' +
		(k?.memory?.relation_count || "-") +
		"</span></div>" +
		'<div style="margin-top:8px">' +
		'<input id="mem-query" placeholder="' +
		__("关键词查询", "Keyword query") +
		'">' +
		'<button class="btn btn-primary btn-sm" onclick="queryMemoryChat()">' +
		__("查询", "Query") +
		"</button>" +
		'</div><div id="mem-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>' +
		"</div></div>";
	html +=
		'<div class="chat-panel" id="chat-panel-context"><div class="card"><h2>' +
		__("上下文", "Context") +
		"</h2>" +
		'<div style="margin-top:8px">' +
		'<input id="ctx-query" placeholder="' +
		__("输入当前话题", "Enter current topic") +
		'">' +
		'<button class="btn btn-primary btn-sm" onclick="queryMemoryContext()">' +
		__("获取上下文", "Get Context") +
		"</button>" +
		'</div><div id="ctx-result" style="margin-top:8px;max-height:200px;overflow:auto"></div>' +
		"</div></div>";
	html +=
		'<div class="chat-panel" id="chat-panel-knowledge"><div class="card"><h2>' +
		__("知识", "Knowledge") +
		"</h2>" +
		'<div class="kv-row"><span class="key">' +
		__("项目", "Items") +
		'</span><span class="val" id="know-count">' +
		(k?.knowledge?.item_count ?? "-") +
		"</span></div>" +
		'<details id="know-tree-box" style="margin-top:8px">' +
		'<summary style="cursor:pointer;font-size:12px;opacity:.8">' +
		__("按分类浏览", "Browse by category") +
		"</summary>" +
		'<div id="know-tree" style="margin-top:6px;max-height:200px;overflow:auto;font-size:12px"></div>' +
		"</details>" +
		'<div style="margin-top:8px;display:flex;gap:4px;flex-wrap:wrap;align-items:center">' +
		'<input id="know-query" placeholder="' +
		__("搜索知识", "Search knowledge") +
		'" style="flex:1;min-width:120px">' +
		'<span id="know-cat-scope" style="font-size:11px;opacity:.7"></span>' +
		'<button class="btn btn-primary btn-sm" onclick="searchKnowledgeChat()">' +
		__("搜索", "Search") +
		"</button>" +
		'<button class="btn btn-sm" onclick="clearKnowledgeCategory()">' +
		__("全库", "All") +
		"</button>" +
		'</div><div id="know-result-chat" style="margin-top:8px;max-height:220px;overflow:auto"></div>' +
		'<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px">' +
		'<input id="know-name" placeholder="' +
		__("知识名称", "Knowledge name") +
		'" style="margin-bottom:4px">' +
		'<textarea id="know-content" placeholder="' +
		__("内容", "Content") +
		'" style="min-height:50px;margin-bottom:4px"></textarea>' +
		'<input type="file" id="know-media" multiple accept="image/*,audio/*,video/*,.md,.txt" style="margin-bottom:6px;font-size:11px">' +
		'<div><button class="btn btn-primary btn-sm" onclick="createKnowledgeChat()">' +
		__("创建", "Create") +
		"</button>" +
		'<span id="know-media-hint" style="margin-left:6px;font-size:11px;opacity:.7"></span>' +
		"</div>" +
		"</div></div></div>";
	html += "</div>";
	cont.innerHTML = html;
	_chatLayoutBuilt = true;
}

var CHAN_COLORS = [
	"#e08a5f",
	"#5f9fe0",
	"#6bbf8f",
	"#c06bbf",
	"#d9a13b",
	"#5fb3bf",
	"#b06b6b",
	"#7f8ce0",
];

function chanColor(src) {
	var h = 0;
	for (var i = 0; i < src.length; i++) {
		h = (h * 31 + src.charCodeAt(i)) >>> 0;
	}
	return CHAN_COLORS[h % CHAN_COLORS.length];
}

function chanLetter(src) {
	var s = (src || "").trim();
	if (!s) return "C";
	var ch = s.charAt(0).toUpperCase();
	return /[A-Za-z0-9]/.test(ch) ? ch : "C";
}

// ===== 列表对账（keyed reconcile）=====
//
// 为什么必须对账而不是 innerHTML 整块重建：整块重建会把**没变的**那些消息
// 节点也推倒重来 —— 图片重新解码闪一下、工具卡片的展开态丢失、CSS 过渡从
// 头播、滚动锚点被重置。这些闪烁几乎无法用"重建后再还原"消除。
//
// 做法：新 HTML 先在游离容器里解析成节点，然后按 data-key 逐个对账：
//   - key 命中且内容一致 → 复用原节点（一个字节都不动）；
//   - key 命中但内容变了 → 只替换这一个节点；
//   - key 未命中 → 作为新节点插入；
// 最后把多出来的旧节点删掉。未变节点因此完全不受影响。
var _localKeySeq = 0;

// chatMsgKey 给一条消息一个**稳定**的 key。
//   - 服务端消息用 seq（单调、落盘、重启不重编号）；
//   - 本地乐观消息（用户刚发的 / 正在流式的）第一次渲染时分配一个进程内
//     自增 key 并挂在对象上，后续渲染不变。
function chatMsgKey(m) {
	if (m && m.seq) return "s" + m.seq;
	if (m && !m._k) m._k = "L" + ++_localKeySeq;
	return (m && m._k) || "L0";
}

function commitChatList(container, html) {
	var tpl = document.createElement("div");
	tpl.innerHTML = html;
	var next = Array.prototype.slice.call(tpl.children);
	var existing = {};
	Array.prototype.forEach.call(container.children, (n) => {
		var k = n.getAttribute && n.getAttribute("data-key");
		if (k) existing[k] = n;
	});
	for (var i = 0; i < next.length; i++) {
		var fresh = next[i];
		var key = fresh.getAttribute && fresh.getAttribute("data-key");
		var node = fresh;
		if (key && existing[key]) {
			var old = existing[key];
			// 内容逐字节相同 → 复用旧节点（不触碰它，保存动画/展开/图片状态）；
			// 否则只替换这一个。outerHTML 比较对消息节点足够（含 tool_calls 状态）。
			if (old.outerHTML === fresh.outerHTML) {
				node = old;
			} else {
				old.remove();
			}
		}
		var cur = container.children[i];
		if (cur !== node) container.insertBefore(node, cur || null);
	}
	// 多出来的旧节点（本轮新列表里没有的）从尾部清掉
	while (container.children.length > next.length) {
		container.removeChild(container.lastElementChild);
	}
}

function renderChat() {
	if (!_chatLayoutBuilt) {
		buildChatLayout();
		renderChatStarmap();
		renderTerminals();
		renderCmdHistory();
	}
	var msgsEl = document.getElementById("chat-msgs");
	if (!msgsEl) return;
	if (!msgsEl._stickBound) {
		msgsEl._stickBound = true;
		msgsEl.addEventListener(
			"scroll",
			() => {
				state.chatStick =
					msgsEl.scrollHeight - msgsEl.scrollTop - msgsEl.clientHeight < 80;
				// 触顶（近顶部 60px）且服务端还有更早历史 → 向上懒加载下一页
				if (msgsEl.scrollTop < 60 && state.chatHasMore) {
					loadOlderChat();
				}
			},
			{ passive: true },
		);
		msgsEl.addEventListener(
			"load",
			() => {
				if (state.chatStick === true) {
					msgsEl.scrollTop = msgsEl.scrollHeight;
				}
			},
			true,
		);
	}
	var msgs = state.messages;
	var sig =
		msgs
			.map((m) => {
				var c = m.content || "";
				return (
					(m.role || "") +
					":" +
					c.length +
					":" +
					c.slice(-40) +
					":" +
					(m.tool_calls || [])
						.map((t) => (t.tool || t.name || "") + "/" + (t.status || ""))
						.join(",")
				);
			})
			.join("|") +
		"|L" +
		(state.chatLoading ? "1" : "0") +
		"|P" +
		(state.pendingTools || []).join(",");
	if (msgsEl._chatSig === sig && msgsEl.childElementCount > 0) {
		if (state.chatStick === true) {
			msgsEl.scrollTop = msgsEl.scrollHeight;
		}
		return;
	}
	msgsEl._chatSig = sig;
	var prevPending = msgsEl._lastPending || [];
	var newPending = (state.pendingTools || []).slice();
	if (state.chatLoading) {
		var lastM = msgs.length ? msgs[msgs.length - 1] : null;
		if (lastM && lastM.role === "assistant") {
			(lastM.tool_calls || []).forEach((tc) => {
				if (!tc.result && tc.status !== "denied") {
					var nm = tc.tool || tc.name || "";
					if (newPending.indexOf(nm) === -1) newPending.push(nm);
				}
			});
		}
	}
	var newlyDone = prevPending.filter((n) => newPending.indexOf(n) === -1);
	msgsEl._lastPending = newPending;
	var streamingLast = !!(
		state.chatLoading &&
		lastM &&
		lastM.role === "assistant" &&
		!lastM._final
	);
	function pillHtml() {
		var s = "";
		newPending.forEach((nm) => {
			var anim = prevPending.indexOf(nm) !== -1 ? "" : " pill-in";
			s +=
				'<span class="thinking-tool' +
				anim +
				'" data-tool="' +
				escHtml(nm) +
				'">' +
				'<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>' +
				escHtml(nm) +
				"</span>";
		});
		return s;
	}
	var html = "";
	if (msgs.length === 0) {
		html =
			'<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' +
			__(
				"开始对话以测试 Agent 回复",
				"Start a conversation to test Agent replies",
			) +
			"</p></div>";
	} else {
		msgs.forEach((m, i) => {
			var _k = chatMsgKey(m);
			var role = m.role || "user";
			var c = m.content || "";
			// 附件消息：agent 经 output_send__webui 发送，或用户上传。
			// 用 role 区分方向（user=右侧你发的 / assistant=左侧小宅发的），
			// 卡片加来源标签避免混淆。
			if (m.attachment) {
				var att = m.attachment;
				var isUserAtt = role === "user";
				var srcLabel = isUserAtt
					? __("你发送的", "You sent")
					: __("小宅发送的", "Sent by agent");
				var attHtml = "";
				if (att.type === "image") {
					attHtml =
						'<a href="' +
						escHtml(att.url) +
						'" target="_blank" rel="noopener">' +
						'<img class="chat-attachment-img" src="' +
						escHtml(att.url) +
						'" ' +
						'alt="image" loading="lazy" style="max-width:320px;max-height:240px;border-radius:10px;display:block;cursor:zoom-in" ' +
						'onerror="this.parentElement.innerHTML=\'<span class=\\"att-err\\">图片加载失败</span>\'"/></a>';
				} else if (att.type === "audio") {
					// 音频用原生播放器：与图片同理，附件能在聊天里直接消费才算可见。
					// preload="metadata" 只拉时长不拉全部字节，避免历史消息满屏时并发下载。
					attHtml =
						'<audio class="chat-attachment-audio" controls preload="metadata" src="' +
						escHtml(att.url) +
						'" style="max-width:320px;display:block"></audio>' +
						'<a href="' +
						escHtml(att.url) +
						'" download style="font-size:12px;color:var(--text-muted,#888);text-decoration:none">' +
						escHtml(att.name || "audio") +
						(att.size ? " (" + formatBytes(att.size) + ")" : "") +
						"</a>";
				} else {
					var sizeStr = att.size ? formatBytes(att.size) : "";
					attHtml =
						'<a class="chat-attachment-file" href="' +
						escHtml(att.url) +
						'" download ' +
						'style="display:inline-flex;align-items:center;gap:8px;padding:8px 14px;border-radius:10px;background:' +
						(isUserAtt
							? "var(--accent-weak, rgba(74,144,217,0.12))"
							: "var(--bg-sec,#f0f2f5)") +
						';text-decoration:none;color:inherit">' +
						'<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M7 10l5 5 5-5M12 15V3"/></svg>' +
						"<span>" +
						escHtml(att.name || "附件") +
						(sizeStr ? " <small>(" + sizeStr + ")</small>" : "") +
						"</span></a>";
				}
				html +=
					'<div class="msg ' +
					(isUserAtt ? "user" : "assistant") +
					'" data-key="' +
					_k +
					'"><div class="msg-bubble"><div class="att-wrap">' +
					'<div style="font-size:11px;opacity:0.65;margin-bottom:4px">' +
					srcLabel +
					"</div>" +
					attHtml +
					(c ? '<div class="text">' + renderMd(c) + "</div>" : "") +
					"</div></div></div>";
				return;
			}
			if (role === "assistant") {
				c = renderMd(c);
			} else if (role === "system") {
				c = escHtml(c);
			} else {
				c = escHtml(c);
			}
			var isChan = !!(m.source && m.source !== "webui");
			var rc = "";
			if (m.reasoning_content) {
				rc = renderReasoningCard(m.reasoning_content, isStreamingLast);
			}
			var tcs = "";
			if (m.tool_calls && m.tool_calls.length > 0) {
				m.tool_calls.forEach((tc) => {
					var argsStr =
						typeof tc.args === "object"
							? JSON.stringify(tc.args, null, 1)
							: tc.args || "";
					var resultStr = tc.result
						? typeof tc.result === "object"
							? JSON.stringify(tc.result, null, 1)
							: String(tc.result)
						: "";
					var running = !resultStr && tc.status !== "denied";
					var error =
						tc.status === "error" || tc.status === "denied" || !!tc.error;
					var drip =
						newlyDone.indexOf(tc.tool || tc.name || "") !== -1
							? " tool-drip-in"
							: "";
					var iconSvg = error
						? '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="9"/><path d="M5.6 5.6l12.8 12.8"/></svg>'
						: running
							? '<span class="tc-spinner"></span>'
							: '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>';
					var statusHtml =
						tc.status === "denied"
							? '<span class="tc-state tc-deny">' +
								__("已拒绝", "Denied") +
								"</span>"
							: running
								? '<span class="tc-state tc-run">' +
									__("调用中", "Running") +
									"</span>"
								: '<span class="tc-state tc-done">' +
									__("完成", "Done") +
									"</span>";
					var pluginHtml = tc.plugin
						? '<span class="tc-plugin">' + escHtml(tc.plugin) + "</span>"
						: "";
					tcs +=
						'<div class="tool-card' +
						(error ? " tc-error" : running ? " tc-running" : " tc-done") +
						drip +
						'" data-tool="' +
						escHtml(tc.tool || tc.name || "") +
						'" onclick="toggleToolCall(this)">' +
						'<div class="tc-line"><span class="tc-ico">' +
						iconSvg +
						'</span><span class="tc-name">' +
						escHtml(tc.tool || tc.name || "") +
						"</span>" +
						pluginHtml +
						statusHtml +
						'<span class="tc-caret">' +
						RT_ICO.caret +
						"</span></div>" +
						'<div class="tc-detail" style="display:none">' +
						(argsStr && argsStr !== "{}"
							? '<div class="tc-args"><div class="tc-detail-label">' +
								__("参数", "Args") +
								"</div>" +
								escHtml(argsStr) +
								"</div>"
							: "") +
						(resultStr
							? '<div class="tc-result"><div class="tc-detail-label">' +
								__("结果", "Result") +
								"</div>" +
								escHtml(resultStr) +
								"</div>"
							: "") +
						"</div></div>";
				});
			}
			var body = rc + tcs;
			var growCls = m._grow ? " grow-in" : "";
			if (m._grow) m._grow = false;
			var isStreamingLast = i === msgs.length - 1 && streamingLast;
			if (isStreamingLast) {
				var liveRow =
					'<span class="live-spinner"></span>' +
					(newPending.length
						? '<span class="thinking-tools">' + pillHtml() + "</span>"
						: "");
				if (c) {
					body +=
						'<div class="msg-bubble' +
						growCls +
						'">' +
						liveRow +
						'<div class="text">' +
						c +
						"</div></div>";
					c = "";
				} else {
					body += '<div class="msg-bubble">' + liveRow + "</div>";
				}
			} else if (c) {
				body +=
					'<div class="msg-bubble' +
					growCls +
					'"><div class="text">' +
					c +
					"</div></div>";
			}
			if (role === "system") {
				html +=
					'<div class="msg msg-system" data-key="' +
					_k +
					'"><div class="msg-bubble">' +
					(c || "") +
					"</div></div>";
			} else if (isChan) {
				html +=
					'<div class="msg msg-channel" data-key="' +
					_k +
					'">' +
					'<div class="msg-avatar chan-avatar" style="background:' +
					chanColor(m.source) +
					'">' +
					chanLetter(m.source) +
					"</div>" +
					'<div class="msg-content"><div class="msg-chan-name">' +
					escHtml(m.source) +
					"</div>" +
					body +
					"</div></div>";
			} else {
				var userAvatar =
					'<svg viewBox="0 0 24 24" style="width:16px;height:16px" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>';
				var aiAvatar = '<img src="/mascot.webp" alt="小宅">';
				html +=
					'<div class="msg msg-' +
					role +
					'" data-key="' +
					_k +
					'">' +
					'<div class="msg-avatar">' +
					(role === "user" ? userAvatar : aiAvatar) +
					"</div>" +
					'<div class="msg-content">' +
					body +
					"</div>" +
					"</div>";
			}
		});
	}
	if (state.chatLoading && !streamingLast) {
		var aiAvatarL = '<img src="/mascot.webp" alt="小宅">';
		html +=
			'<div class="msg msg-assistant" data-key="pending"><div class="msg-avatar">' +
			aiAvatarL +
			'</div><div class="msg-content"><div class="msg-bubble">' +
			'<span class="live-spinner"></span>' +
			(newPending.length
				? '<span class="thinking-tools">' + pillHtml() + "</span>"
				: "") +
			"</div></div></div>";
	}
	// 重建前记住阅读位置：非粘底（用户正向上翻）时，innerHTML 重建后必须把位置还回去，
	// 否则视口会被重置——这就是"聊天记录跳到顶部"的直接来源。
	var prevTop = msgsEl.scrollTop;
	commitChatList(msgsEl, html);
	if (state.chatStick !== false) {
		msgsEl.scrollTop = msgsEl.scrollHeight;
	} else {
		msgsEl.scrollTop = prevTop;
	}
	updateChatBadge();
}

function updateChatBadge() {
	var badge = document.getElementById("chat-stage");
	if (!badge) return;
	badge.textContent = state.chatStage || "";
	badge.style.display = "none";
}

// 流式渲染分发：流式增量（只更新最后一条正文）vs 全量重建
var _rerenderTimer = null;
function rerenderChat(full) {
	// 流式中且非强制全量：走增量路径（防抖合并 chunk，只更新最后一条消息节点）
	if (!full && state.chatLoading) {
		var msgs = state.messages;
		var last = msgs.length ? msgs[msgs.length - 1] : null;
		if (last && last.role === "assistant" && !last._final) {
			if (_rerenderTimer) return; // 已有排程的增量更新
			_rerenderTimer = setTimeout(() => {
				_rerenderTimer = null;
				renderChatStreamChunk();
			}, 90);
			return;
		}
	}
	// 非流式（完成/工具/历史变化）：全量重渲（含防抖合并）
	if (_rerenderTimer) clearTimeout(_rerenderTimer);
	_rerenderTimer = setTimeout(() => {
		_rerenderTimer = null;
		renderChat();
		if (full) {
			renderChatStarmap();
			renderTerminals();
			renderCmdHistory();
		}
	}, 90);
}

// 流式增量渲染（移植自 GUI）：仅更新最后一条 assistant 消息的正文与思考预览，
// 不重建 DOM。正文节流 parse（>200 字符或 >300ms 才 renderMd），小增量纯文本追加。
function renderChatStreamChunk() {
	var msgsEl = document.getElementById("chat-msgs");
	var last = state.messages.length
		? state.messages[state.messages.length - 1]
		: null;
	if (!msgsEl || !last) return;
	var el = msgsEl.lastElementChild;
	if (!el) {
		renderChat();
		return;
	}
	var textEl = el.querySelector(".msg-bubble .text");
	var c = last.content || "";
	if (textEl && c) {
		var now = Date.now();
		var lastParse = el.__lastParse || 0;
		var lastLen = el.__lastLen || 0;
		if (c.length - lastLen > 200 || now - lastParse > 300) {
			textEl.innerHTML = renderMd(c);
			el.__lastParse = now;
			el.__lastLen = c.length;
		} else {
			var tail = c.slice(lastLen);
			if (tail) textEl.appendChild(document.createTextNode(tail));
			el.__lastLen = c.length;
		}
		if (state.chatStick !== false) {
			try {
				msgsEl.scrollTop = msgsEl.scrollHeight;
			} catch (e) {}
		}
		return;
	}
	// 思考预览更新（流式中折叠，只刷 preview 文本）
	var rcPrev = el.querySelector(".reasoning-preview");
	if (rcPrev && last.reasoning_content) {
		rcPrev.textContent = last.reasoning_content
			.replace(/[\s\n]+/g, " ")
			.slice(0, 60);
		if (state.chatStick !== false) {
			try {
				msgsEl.scrollTop = msgsEl.scrollHeight;
			} catch (e) {}
		}
		return;
	}
	// 结构变化兜底：全量
	renderChat();
}
function toggleToolCall(el) {
	var d = el.querySelector(".tc-detail");
	if (!d) return;
	var open = d.style.display !== "none";
	d.style.display = open ? "none" : "block";
	if (open) {
		el.classList.remove("open");
	} else {
		el.classList.add("open");
	}
}

function renderReasoningCard(text, isStreaming) {
	var preview =
		typeof marked !== "undefined"
			? text.replace(/[\s\n]+/g, " ").slice(0, 60)
			: escHtml(text)
					.replace(/<[^>]+>/g, " ")
					.slice(0, 60);
	return (
		'<div class="reasoning-card' +
		(isStreaming ? " rc-streaming" : "") +
		'">' +
		'<div class="reasoning-head" onclick="toggleReasoning(this)">' +
		'<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="rc-ico"><path d="M9 3a2 2 0 0 0-2 2v2a2 2 0 0 1-2 2H3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2h2a2 2 0 0 1 2 2v2a2 2 0 0 0 2 2h1a2 2 0 0 0 2-2v-2a2 2 0 0 1 2-2h2a2 2 0 0 0 2-2V7a2 2 0 0 0-2-2h-2a2 2 0 0 1-2-2V3a2 2 0 0 0-2-2H9zM12 8v4m0 4h.01"/></svg>' +
		'<span class="rc-title">' +
		(isStreaming ? __("思考中...", "Thinking...") : __("思考", "Thinking")) +
		"</span>" +
		'<span class="rc-chev">' +
		RT_ICO.caret +
		"</span></div>" +
		'<div class="reasoning-body" style="display:' +
		(isStreaming ? "block" : "none") +
		'">' +
		(isStreaming
			? '<div class="reasoning-preview">' +
				escHtml(preview) +
				'</div><div class="reasoning-sweep"></div>'
			: '<div class="reasoning-content">' + renderMd(text) + "</div>") +
		"</div></div>"
	);
}
function toggleReasoning(el) {
	var card = el.closest(".reasoning-card");
	if (!card) return;
	var body = card.querySelector(".reasoning-body");
	if (!body) return;
	var open = body.style.display !== "none";
	body.style.display = open ? "none" : "block";
	if (open) {
		card.classList.remove("open");
	} else {
		card.classList.add("open");
	}
}

// renderChatStarmap：把星图挂到**当前活跃的容器**上。
// 容器优先级：独立星图页签 > 总览页小图 > 聊天面板。
// 同一套 three.js renderer 在容器间搬运 canvas，不重建场景。
function renderChatStarmap() {
	var cont = starmapActiveContainer();
	if (!cont) return;
	if (
		window._THREE_FAILED ||
		(!window.THREE && window._THREE_FAILED !== undefined)
	) {
		cont.innerHTML =
			'<p style="color:var(--text-muted);padding:20px;text-align:center;font-size:11px">' +
			__(
				"3D 星图不可用（CDN 加载失败）",
				"Star map unavailable (CDN load failed)",
			) +
			"</p>";
		state.starmapInit = true;
		state.starmapLoading = false;
		return;
	}
	if (!window.THREE) {
		cont.innerHTML =
			'<div style="display:flex;align-items:center;justify-content:center;height:100%;padding:20px"><div class="loading-spinner"></div></div>';
		state.starmapInit = false;
		state.starmapLoading = false;
		return;
	}
	if (cont.querySelector("canvas")) {
		var rect = cont.getBoundingClientRect();
		if (starmapRen && rect.width > 0)
			starmapRen.setSize(rect.width, Math.max(rect.height, 250));
		return;
	}
	if (state.starmapInit) {
		if (starmapRen) {
			var rect = cont.getBoundingClientRect();
			if (rect.width > 0)
				starmapRen.setSize(rect.width, Math.max(rect.height, 250));
			cont.appendChild(starmapRen.domElement);
			starmapRen.domElement.style.display = "block";
		} else {
			// starmapRen was destroyed (e.g. re-render cycle), restart
			state.starmapInit = false;
			state.starmapLoading = false;
		}
		return;
	}
	if (state.starmapLoading) return;
	// ★★ 2026-10-13：星图容器不可见时不建图。
	//
	// 为何必须在这里拦（而不是只靠 starmapAnimate 的可见性守卫）：
	//   starmapActiveContainer() 的最后一行 fallback 到 **sm-container-chat**
	//   （聊天面板）。而 starmapAnimate 的渲染循环有 `cont.offsetParent` 守卫，
	//   唯独建图没有 —— 于是“在默认的聊天页签上、根本没打开星图”也会
	//   跑完整套建图（含 10.9 秒的布局）。实测：页面加载后首帧就卡 11.8 秒。
	//
	//   offsetParent 为 null 时容器不可见（display:none 或未挂载）⇒ 不建。
	//   等用户真的点到星图页签，renderChatStarmap / starmapWake 会重新进来。
	var vis = starmapActiveContainer();
	if (!vis || !vis.offsetParent) {
		state.starmapLoading = false;
		// 不置 starmapInit：图还没建，下次可见时还要再进这里。
		state.starmapInit = false;
		return;
	}
	state.starmapLoading = true;
	loadChatStarmapData();
}

// smAdaptGraph 把后端图视图归一化成星图使用的 nodes/edges。
//
// ★ 为何需要适配（2026-10-08）：后端 `GraphData()` 同时返回**两套**数据 ——
//
//	nodes/edges              ← 旧表 entities/relations（2026-10-04 已停写）
//	memory_blocks/block_edges ← 块/边体系（真实活跃的记忆）
//
// 而前端只读了前者 ⇒ 星图**结构性看不到新记忆**（生产实测：旧表停在 10-03，
// 块表同期从 1294 涨到 3178）。这是「记忆系统已迁移、前端没跟上」的典型残留。
//
// 归一化后的节点字段与旧格式**保持一致**（id/name/type/mention_count），
// 这样建图、布局、图例、脉冲那些已有逻辑不需各自判断数据源。
function smAdaptGraph(d) {
	var blocks = d.memory_blocks || [];
	var bEdges = d.memory_block_edges || [];
	if (!blocks.length) {
		// 后端仍是旧格式（或块表空）：照旧渲染，不报错。
		return { nodes: d.nodes || [], edges: d.edges || [] };
	}

	// 1) 节点：块 → 星图节点
	var nodes = blocks.map((b) => {
		var text = String(b.text || "")
			.replace(/\s+/g, " ")
			.trim();
		// 媒体块没有正文（只有 digest/描述）——用工具名与模态拼一个可读名，
		// 否则它们全叫「」在图上无法区分。
		var name = text;
		if (!name) {
			var mod = b.modality || "block";
			name =
				mod === "image"
					? __("图像", "image")
					: mod === "audio"
						? __("音频", "audio")
						: __("记录", "record");
			if (b.tool) name += " · " + b.tool;
		}
		return {
			id: b.id,
			name: name,
			// type 用于配色/图例：媒体块按 modality 归类，文本块按语义类型，
			// 都没给就用 source（sentence / triple / legacy-entity）——比全填
			// "Concept" 更能看出图里长什么。
			type: b.semantic_type || b.modality || b.source || "block",
			mention_count: 0, // 稍后按度数回填
			source: b.source || "",
			modality: b.modality || "",
			scene: b.scene || "",
			created_at: b.created_at,
			updated_at: b.updated_at,
			_block: true,
		};
	});

	// 2) 边：只保留两端都在节点集里的边（端点缺失的边画不出来，
	//    留着只会让度数统计偏大）。
	var byID = {};
	nodes.forEach((n) => {
		byID[n.id] = n;
	});
	var edges = [];
	for (var i = 0; i < bEdges.length; i++) {
		var e = bEdges[i];
		if (!byID[e.source_id] || !byID[e.target_id]) continue;
		// 只画活跃边：deleted/merged 的边留着只会让已失效的关系看着还在。
		if (e.status && e.status !== "active" && e.status !== "") continue;
		edges.push({
			source_id: e.source_id,
			target_id: e.target_id,
			relation_type: e.type || "",
		});
	}

	// 3) mention_count 回填为**度数**。
	//
	// 块体系里没有 mention_count 这一列（那是旧实体表的概念），而星图的
	// 节点大小/布局/标签优先级都靠它。度数是最好的替代：被引用越多的块
	// 越大 —— 与「越常被提起的实体越大」是同一个直觉。
	edges.forEach((e) => {
		byID[e.source_id].mention_count++;
		byID[e.target_id].mention_count++;
	});

	return { nodes: nodes, edges: edges };
}

async function loadChatStarmapData() {
	try {
		var resp = await api("/memory/graph");
		// 星图现在有三个容器（总览/独立页签/聊天面板），写提示前必须重新
		// 拿当前活跃的那个。原代码硬写 getElementById("sm-container-chat")，
		// 而总览页与独立页签都没有这个 id —— 空图分支一进就抛
		// 「Cannot set properties of null」（浏览器实测：首页首帧必现）。
		// 症状是整个函数被 catch 吞掉、state.starmapInit 却没置上，
		// 于是后续再也不会重试 —— 星图永远是空的。
		var target = starmapActiveContainer();
		if (!resp || !resp.success || !resp.data) {
			if (target)
				target.innerHTML =
					'<p style="color:var(--text-muted);padding:20px;text-align:center">' +
					__("暂无记忆数据", "No memory data") +
					"</p>";
			state.starmapInit = true;
			state.starmapLoading = false;
			return;
		}
		// 空图判据取 memory_blocks 而不是 nodes。
		//
		// ★ 为何换源（2026-10-08）：`nodes` 读的是**旧表 entities**，而它自
		//   2026-10-04 停双写后不再增长（生产实测最后写入停在 10-03）。
		//   真实记忆全在 memory_blocks（同期 1294 → 3178 仍在涨）。
		//   继续读 nodes 等于星图永远看不到新记忆。
		var blocks = resp.data.memory_blocks || [];
		if (blocks.length === 0 && (resp.data.nodes || []).length === 0) {
			if (target)
				target.innerHTML =
					'<p style="color:var(--text-muted);padding:20px;text-align:center">' +
					__("暂无记忆数据", "No memory data") +
					"</p>";
			state.starmapInit = true;
			state.starmapLoading = false;
			return;
		}
		var d = resp.data;
		// smAdaptGraph 把块/边视图归一化成星图节点/边。
		// 回退分支保留：若后端仍是旧格式（仅 nodes/edges），照旧渲染。
		var norm = smAdaptGraph(d);
		starmapNodes = norm.nodes;
		starmapEdges = norm.edges;
		state.starmapInit = true;
		state.starmapLoading = false;
		initChatStarmap();
	} catch (e) {
		var target2 = starmapActiveContainer();
		if (target2)
			target2.innerHTML =
				'<p style="color:var(--text-muted);padding:20px;text-align:center">' +
				__("加载失败", "Load failed") +
				"</p>";
		state.starmapInit = true;
		state.starmapLoading = false;
	}
}

function initChatStarmap() {
	// 容器可以是独立星图页签 / 总览页 / 聊天面板 —— 同一套 renderer
	// 会在多个容器间搬运 canvas（appendChild 即可）。
	var cont = starmapActiveContainer();
	if (!cont) return;
	var rect = cont.getBoundingClientRect();
	var w = Math.max(rect.width || 300, 100);
	var h = Math.max(rect.height || 250, 100);
	if (starmapRen) {
		starmapRen.setSize(w, h);
		// 换父节点前先把旧角标从旧容器里拨掉，否则它会残留在旧位置。
		if (starmapLabelEl && starmapLabelEl.parentElement === cont) {
			starmapLabelEl.style.display = "none";
		}
		cont.appendChild(starmapRen.domElement);
		starmapRen.domElement.style.display = "block";
		return;
	}
	starmapScene = new THREE.Scene();
	starmapScene.fog = new THREE.FogExp2(0x0a0a1a, 0.015);
	starmapCam = new THREE.PerspectiveCamera(60, w / h, 0.1, 2000);
	// 初始距离随节点规模自适应：固定 (0,20,40) 是为 1294 节点时代调的，
	// 换到 3178 节点（半径最大 240+）后这个机位只能看到中心一小块。
	// 用 maxR 估算一个能看到全景的距离，并留 1.6 倍余量。
	var fitDist = smStarmapFitDistance(w, h);
	starmapCam.position.set(0, fitDist * 0.45, fitDist * 0.9);
	starmapRen = new THREE.WebGLRenderer({ antialias: true, alpha: true });
	starmapRen.setSize(w, h);
	starmapRen.setPixelRatio(Math.min(window.devicePixelRatio, 2));
	starmapRen.setClearColor(0x0a0a1a, 1);
	cont.innerHTML = "";
	cont.appendChild(starmapRen.domElement);
	starmapCtrl = new THREE.OrbitControls(starmapCam, starmapRen.domElement);
	starmapCtrl.enableDamping = true;
	starmapCtrl.dampingFactor = 0.05;
	starmapCtrl.rotateSpeed = 0.5;
	starmapCtrl.zoomSpeed = 0.8;
	var al = new THREE.AmbientLight(0x444466, 0.6);
	starmapScene.add(al);
	var dl = new THREE.DirectionalLight(0xffffff, 0.8);
	dl.position.set(50, 100, 50);
	starmapScene.add(dl);
	createStarField();
	createNebula();
	// 共享几何（低规格）：所有节点/光晕球复用这两份。
	//
	// ★ 细分从 (8,6) 降到 (6,4)：3753 个节点 × 2（主球+光晕实例）时，
	//   (8,6) 球体约 84 三角形/个 ⇒ 60 万顶点变换/帧；(6,4) 约 36 ⇒
	//   降到约 26 万。节点球在屏上半径本就只有 0.5–2.5 单位，这档细分
	//   看不出区别，而顶点变换是软件渲染环境下的主要成本。
	starmapGeo = new THREE.SphereGeometry(0.5, 6, 4);
	starmapGlowGeo = new THREE.SphereGeometry(1.0, 6, 4);
	// mesh 集合即将重建 ⇒ 标签计划缓存失效（baseVer 是缓存的唯一失效信号）。
	starmapLabelBaseVer++;
	starmapLabelPlanCache = null;
	buildChatStarmapGraph();
	starmapRen.domElement.addEventListener("mousemove", onStarmapMove);
	starmapRen.domElement.addEventListener("click", onStarmapClick);
	window.addEventListener("resize", onStarmapResize);
	// 用 starmapWake（带开关）而不是裸调 starmapAnimate：
	// 避免重复点火时留下两个并行的 RAF 链（那会让每帧工作量翻倍）。
	starmapWake();
	// 活动数据源：/runtime 3s + /memory/graph/pulse 10s。
	// 只在星图真正初始化后启动，避免在隐藏页签空跑。
	starmapStartActivity();
	// 图例按实际出现的类型生成（建图后 smPresentTypes 才是准的）。
	smLegend();
	smUpdateStat();
}

// starmapStartActivity 启动两路活动轮询（幂等）。
function starmapStartActivity() {
	if (state.starmapPulseTimer) return;
	starmapPullActivity();
	starmapPullPulse();
	state.starmapPulseTimer = setInterval(() => {
		starmapPullActivity();
	}, 3000);
	var pulseTimer = setInterval(() => {
		starmapPullPulse();
		// ★ 消费 starmapDirty（图结构变了就重拉全量）。
		//
		// 为何必须在这里消费：starmapDirty 此前**只被赋值、从未被读取**
		// （且未声明 —— 赋值一个隐式全局），于是新写入的记忆块**永远不会**
		// 出现在星图上，即使后端已经在发 memory_access 事件。
		// 这是「图不跟着变」的最后一个断点。
		if (starmapDirty) {
			starmapDirty = false;
			// 全量重拉会重建整个场景；图不可见时跳过，避免白白重建。
			//
			// ★ 节流：生产库 3178 块时 /memory/graph 响应体实测 **3.3MB**（压测库 7144 块
			//   时更大）。若记忆密集写入（一次 batch commit 会连发十几个事件），
			//   每 10s 重拉一次 3.3MB 是纯浪费——而 90% 的重拉内容与上次一样。
			//   因此最少间隔 30s；写入稀少时（生产实测 27 次/8.5h）实际上很少触发。
			var nowMs = Date.now();
			if (
				starmapActiveContainer() &&
				!state.starmapLoading &&
				nowMs - (state.starmapLastFullReload || 0) > 30000
			) {
				state.starmapLastFullReload = nowMs;
				state.starmapInit = false;
				loadChatStarmapData();
			}
		}
	}, 10000);
	// 两个 id 合并到一个字段会导致 setInterval 被覆盖，这里分开记。
	state.starmapActivityTimer = pulseTimer;
}

function buildChatStarmapGraph() {
	// 先统计实际出现的类型（smColorFor / 动态图例都要用）。
	// 必须在建节点**之前**算完 —— 颜色在创建 mesh 时就定下了。
	smPresentTypes = {};
	starmapNodes.forEach((n) => {
		var t = String(n.type || "").toLowerCase() || "(未知)";
		smPresentTypes[t] = (smPresentTypes[t] || 0) + 1;
	});
	starmapNodeMeshes.forEach((m) => {
		starmapScene.remove(m);
	});
	starmapEdgeLines.forEach((l) => {
		starmapScene.remove(l);
	});
	starmapNodeMeshes = [];
	starmapEdgeLines = [];
	// ★ 必须同时清空形变活跃集（2026-10-08）：上面刚把旧 mesh 从场景摘掉，
	//   而它们可能仍在 starmapMorphSet 里。若不清，这个集合会随每次重建图
	//   单调增长（永不归零），于是「空集合时 O(1) 返回」的快速路径永远失效，
	//   每帧又要遍历一批**已经不在场景里**的废弃 mesh —— 正好把本次优化
	//   的努力又还回去了。
	if (starmapMorphSet) starmapMorphSet.clear();
	if (starmapNodes.length === 0) return;
	// Calculate node degrees for leaf node detection
	var nodeDegs = {};
	starmapNodes.forEach((n) => {
		nodeDegs[n.id] = 0;
	});
	starmapEdges.forEach((e) => {
		nodeDegs[e.source_id] = (nodeDegs[e.source_id] || 0) + 1;
		nodeDegs[e.target_id] = (nodeDegs[e.target_id] || 0) + 1;
	});
	var nodeMap = {};
	starmapNodes.forEach((n) => {
		nodeMap[n.id] = n;
	});
	var sorted = starmapNodes
		.slice()
		.sort((a, b) => (b.mention_count || 0) - (a.mention_count || 0));
	var mc = sorted.map((n) => n.mention_count || 0);
	var maxMc = Math.max(...mc, 1),
		minMc = Math.min(...mc, 0),
		rng = maxMc - minMc || 1;
	// Layout positions
	var pos = {};
	// ★ 半径随节点数自适应（2026-10-08）。
	//
	// 原来是写死的 baseR=15 / maxR=80。而换到块体系后节点从 1294 增到 3178，
	// 同样一个环带里塞两倍多的节点 ⇒ 节点间平均间距降到不足节点自身半径，
	// 视觉上挤成一团 —— 这是“可读性差”的直接成因之一。
	//
	// 做法：保持**面密度**恒定 —— 环带面积随节点数线性增长，
	// 于是半径按 √n 缩放。这样不管 1 千还是 1 万节点，
	// 屏上的疏密观感一致，不会因为记忆变多而糊掉。
	var baseR = 15;
	// 每个节点预留的面积（单位²）。取值来自旧布局的实测密度：
	// 1294 节点 / π(80²-15²) ≈ 0.067 节点/单位² ⇒ 每节点约 15 单位²。
	// 略留宽一点（18）给标签留位置。
	var maxR = Math.sqrt((sorted.length * 18) / Math.PI + baseR * baseR);
	// 上限防极端：几万节点时半径会大到镜头看不到全景。
	if (maxR > 260) maxR = 260;
	var total = sorted.length;
	var acc = 0;
	sorted.forEach((n, i) => {
		var m = n.mention_count || 0,
			mn = rng > 0 ? (m - minMc) / rng : 0;
		var radius = baseR + mn * (maxR - baseR);
		var baseStep = (Math.PI * 2) / total;
		var extra = mn * baseStep * 2;
		var angle = acc + extra / 2;
		acc += baseStep + extra;
		pos[n.id] = {
			x: radius * Math.cos(angle),
			y: (Math.random() - 0.5) * (10 + mn * 20),
			z: radius * Math.sin(angle),
			mn: mn,
			rad: radius,
		};
	});
	// Leaf nodes (degree 1) reposition near parent
	sorted.forEach((n) => {
		var deg = nodeDegs[n.id] || 0;
		if (deg !== 1) return;
		var edge = starmapEdges.find(
			(e) => e.source_id === n.id || e.target_id === n.id,
		);
		if (!edge) return;
		var parentId = edge.source_id === n.id ? edge.target_id : edge.source_id;
		if (!pos[parentId]) return;
		var pp = pos[parentId];
		var m = n.mention_count || 0,
			mn = rng > 0 ? (m - minMc) / rng : 0;
		var off = 6 + mn * 8 + Math.random() * 4;
		var a2 = Math.random() * Math.PI * 2;
		pos[n.id] = {
			x: pp.x + off * Math.cos(a2),
			y: pp.y + (Math.random() - 0.5) * (4 + mn * 6),
			z: pp.z + off * Math.sin(a2),
			mn: mn,
			rad: off,
		};
	});
	// Force-directed simulation
	//
	// ★★ 2026-10-13：O(n²) → O(n·k) 网格法（relex 改动见 starmapLayoutRelax）。
	//
	// 旧实现在这里内联跑 50 轮 `for i / for j=i+1` 全对全斥力。生产库
	// 3754 个块时每轮 704 万次配对、50 轮 3.52 亿次 —— CDP Profiler 实测
	// **单次阻塞主线程 10.9 秒**（页面首帧 11.8 秒）。
	//
	// 而那段斥力体里本来就有 `if (d < 25)` —— 只对近邻施力。既然只关心
	// 近邻，就没必要算全对全：把空间切成边长 == 斥力半径的格子，每点只与
	// 自身格 + 相邻 26 格比较即可（距离 < 25 的点必然落在相邻格内，不遗漏）。
	//
	// 现在这段计算搬进了 Web Worker（static/starmap-layout-worker.js），
	// 主线程在等待期间不阻塞。**同一个算法在两边各有一份**（此处为串行
	// 回退），刻意保持一致 —— 若回退走旧实现，两条路径会给出不同布局。
	var flat = new Float64Array(starmapNodes.length * 3);
	for (var li = 0; li < starmapNodes.length; li++) {
		var lp = pos[starmapNodes[li].id];
		flat[li * 3] = lp.x;
		flat[li * 3 + 1] = lp.y;
		flat[li * 3 + 2] = lp.z;
	}
	/* 把扁平坐标写回 pos —— Worker 回传与串行回退共用。 */
	function applyFlat(arr) {
		for (var i = 0; i < starmapNodes.length; i++) {
			var p = pos[starmapNodes[i].id];
			if (!p) continue;
			p.x = arr[i * 3];
			p.y = arr[i * 3 + 1];
			p.z = arr[i * 3 + 2];
		}
	}
	if (starmapLayoutWorkerAvailable()) {
		// 异步：先把节点画出来（初始环带摆位已可用），布局在后台算，
		// 算完再原地更新 mesh 位置。用户立刻看到图，随后图自己舒展开。
		starmapRequestLayout(flat, maxR, nodeDegs, (arr) => {
			applyFlat(arr);
			for (var k = 0; k < starmapNodeMeshes.length; k++) {
				var p2 = pos[starmapNodeMeshes[k].userData.nodeId];
				if (p2) starmapNodeMeshes[k].position.set(p2.x, p2.y, p2.z);
			}
			// 边与光晕都烧进了位置（LineSegments 顶点 / 实例矩阵），
			// 节点一动就得重建。
			starmapRebuildEdgePositions(pos);
		});
	} else {
		// 回退：无 Worker（file://、老浏览器、构造失败）时同步算。
		// 与 Worker 同一算法、同一迭代策略，只是占主线程。
		starmapLayoutRelax(flat, starmapNodes, starmapEdges, maxR);
		applyFlat(flat);
	}
	// Create nodes
	//
	// ★ 低规格渲染（1151 节点实测后定的方案）：保留**全部**节点，
	// 但把每节点开销从「一个独立球 + 一个独立球光晕 + 一张 256x64
	// 文字贴图」降到「共享几何 + 共享材质 + 无常驻文字」。
	//
	// 改之前的实际开销（生产实例 1151 节点）：
	//   - SphereGeometry(16,12) 主球 + 同样规格的光晕球 = 2302 个独立
	//     BufferGeometry，共约 88 万三角形
	//   - 每节点一张 256x64 CanvasTexture = 1151 张 <canvas> +
	//     1151 个纹理，仅文字就吃约 72MB 显存
	// 这两样在「星图只是个展示」的前提下纯属浪费：星图全图远看根本
	// 读不清标签（256x64 贴在半径 0.5~2.5 的球上，本来就糊）。
	//
	// 现在：共享一份 SphereGeometry(8,6)（约 84 三角形/节点），
	// 颜色靠每 mesh 的 material.color（材质本身仍每节点一份，
	// 因为 MeshPhongMaterial 要独立发光强度才能做活动脉冲）。
	// 标签改为「hover / 选中时才在容器角上显示 HTML 文本」——
	// 文字清晰度反而比 3D 贴图好，且零显存。
	// 主球仍每节点一个（材质独立才能做活动脉冲），但**去掉光晕子节点**，
	// 光晕收进一个 InstancedMesh（见 starmapBuildGlowInstances）。
	if (starmapGlowMesh) {
		starmapScene.remove(starmapGlowMesh);
		if (starmapGlowMesh.geometry) starmapGlowMesh.geometry.dispose();
		if (starmapGlowMesh.material) starmapGlowMesh.material.dispose();
		starmapGlowMesh = null;
	}
	var glowItems = [];
	starmapNodes.forEach((n) => {
		var p = pos[n.id];
		if (!p) return;
		var mn = n.mention_count || 0,
			mnr = rng > 0 ? (mn - minMc) / rng : 0;
		var rad = 0.5 + mnr * 2.0;
		// 灰色调：亮度按重要度（度数归一化）——亮处即记忆稠密处。
		// 旧实现是「按类型上色 + 小写归一化」，而真实图谱 99% 同类型，
		// 那套配色在生产数据上实质只用得上一种颜色。
		var col = smColorFor(n.type, mnr);
		var ei = 0.3 + mnr * 0.7;
		var mat = new THREE.MeshPhongMaterial({
			color: col,
			emissive: col,
			emissiveIntensity: ei,
			shininess: 30,
		});
		var mesh = new THREE.Mesh(starmapGeo, mat);
		mesh.position.set(p.x, p.y, p.z);
		mesh.scale.setScalar(rad / 0.5); // 共享几何半径 0.5，按需缩放
		mesh.userData.nodeData = n;
		mesh.userData.nodeId = n.id;
		mesh.userData.baseEmissive = ei;
		mesh.userData.baseScale = rad / 0.5;
		mesh.userData.baseGlow = 0.12 + mnr * 0.08;
		// ★★ 2026-10-13：光晕球不再每节点一个 Mesh。
		//
		// 原先 3753 个节点 = 3753 个主球 + 3753 个光晕球 = 7506 个独立 mesh。
		// 实测 renderer.info.render.calls = **7512 draw call/帧**（另一台机子
		// 上主线程 100% 忙）。而光晕球是**纯静态装饰**——全仓 grep 显示
		// `glowSphere` 只有写入、从无读取，任何动画都不碰它。
		//
		// 因此可以安全合并成单个 InstancedMesh（3753 → 1 draw call）：
		// 同一几何 + 同一材质，每个实例只差「位置/缩放/颜色」。
		// ★ 不能用每节点材质做透明度：InstancedMesh 只有**一个**材质，
		//   所以统一用固定 opacity（视觉差异极小，而收益是一半 draw call）。
		glowItems.push({
			id: n.id,
			scale: rad * 1.2 + mnr * 0.5,
			color: col,
		});
		// 名字不再烘成贴图；hover 时由 onStarmapMove 写进角标。
		starmapScene.add(mesh);
		starmapNodeMeshes.push(mesh);
	});
	starmapBuildGlowInstances(glowItems, pos);
	// Create edges
	//
	// 866 条边原本每条一个 BufferGeometry + Line + LineBasicMaterial
	// （866 个 draw call）。改为**按关系类型分组**的少量 LineSegments：
	// 同类型边合并成一个几何体，draw call 从 866 降到「关系类型数」
	// （生产实例实测 6 种左右）。
	//
	// 副作用：单条边不再能单独点选。星图此前也没有点选边的交互
	// （onStarmapClick 只处理节点），所以这是纯粹的成本削减。
	starmapBuildEdges(pos);
}

// starmapBuildGlowInstances 把全部节点的光晕球合并成一个 InstancedMesh。
//
// ★★ 为何要合并（2026-10-13 实测）：3753 个节点各自一个光晕 mesh
//   ⇒ renderer.info.render.calls = **7512 次/帧**（主球 3753 + 光晕 3753
//   + 少量边/星空）。在软件渲染环境下这会把主线程占满。
//   合并后 calls 降到约 3759 —— 光晕从 3753 次降到 **1 次**。
//
// 为何能安全合并：光晕是纯静态装饰。全仓 grep 确认 `glowSphere` 只有写入、
// 从无读取，任何动画（脉冲/形变/呼吸）都只动主球材质 —— 所以光晕可以
// 彻底脱离每节点 mesh 体系。
//
// ★ 代价：InstancedMesh 只有**一个**材质，所以每实例的透明度差异没了，
//   统一用固定 opacity（0.16，落在原 baseGlow 区间 0.12–0.20 的中间）。
function starmapBuildGlowInstances(items, pos) {
	if (!items.length || !starmapGlowGeo) return;
	starmapGlowItems = items;
	var gm = new THREE.MeshBasicMaterial({
		transparent: true,
		opacity: 0.16,
		side: THREE.BackSide,
		blending: THREE.AdditiveBlending,
	});
	var inst = new THREE.InstancedMesh(starmapGlowGeo, gm, items.length);
	inst.instanceMatrix.setUsage(THREE.StaticDrawUsage);
	var dummy = new THREE.Object3D();
	var col = new THREE.Color();
	for (var i = 0; i < items.length; i++) {
		var it = items[i];
		var p = pos[it.id];
		if (!p) {
			// 位置缺失时把实例缩到 0（不可见），而不是跳过后留下未初始化
			// 的实例矩阵（那会堆在原点形成一坨亮斑）。
			dummy.position.set(0, 0, 0);
			dummy.scale.setScalar(0);
		} else {
			dummy.position.set(p.x, p.y, p.z);
			dummy.scale.setScalar(it.scale);
		}
		dummy.updateMatrix();
		inst.setMatrixAt(i, dummy.matrix);
		inst.setColorAt(i, col.setHex(it.color));
	}
	inst.instanceMatrix.needsUpdate = true;
	if (inst.instanceColor) inst.instanceColor.needsUpdate = true;
	inst.frustumCulled = false;
	starmapScene.add(inst);
	starmapGlowMesh = inst;
}

// starmapRebuildGlowInstances 用当前坐标重写光晕实例矩阵。
function starmapRebuildGlowInstances(pos) {
	if (!starmapGlowMesh || !starmapGlowItems.length) return;
	var inst = starmapGlowMesh;
	var dummy = new THREE.Object3D();
	var col = new THREE.Color();
	for (var i = 0; i < starmapGlowItems.length && i < inst.count; i++) {
		var it = starmapGlowItems[i];
		var p = pos[it.id];
		if (!p) {
			dummy.position.set(0, 0, 0);
			dummy.scale.setScalar(0);
		} else {
			dummy.position.set(p.x, p.y, p.z);
			dummy.scale.setScalar(it.scale);
		}
		dummy.updateMatrix();
		inst.setMatrixAt(i, dummy.matrix);
		inst.setColorAt(i, col.setHex(it.color));
	}
	inst.instanceMatrix.needsUpdate = true;
	if (inst.instanceColor) inst.instanceColor.needsUpdate = true;
}

// starmapBuildEdges 按关系类型分组，把边建成少量 LineSegments。
//
// ★ 单独抽出来的原因（2026-10-13）：Worker 异步算完布局后节点位置变了，
//   已建好的边几何体会留在旧坐标上（图看着“线错位”）。所以需要一个
//   可重复调用的构建函数，而不是内联在 buildChatStarmapGraph 里。
function starmapBuildEdges(pos) {
	starmapEdgeLines.forEach((l) => {
		starmapScene.remove(l);
		if (l.geometry) l.geometry.dispose();
		if (l.material) l.material.dispose();
	});
	starmapEdgeLines = [];
	var edgeByColor = {};
	starmapEdges.forEach((e) => {
		var a = pos[e.source_id],
			b = pos[e.target_id];
		if (!a || !b) return;
		var col = smEdgeColors[e.relation_type] || smEdgeColors[e.type] || 0x444466;
		var k = String(col);
		if (!edgeByColor[k]) edgeByColor[k] = { col: col, pts: [] };
		edgeByColor[k].pts.push(a.x, a.y, a.z, b.x, b.y, b.z);
	});
	Object.keys(edgeByColor).forEach((k) => {
		var g = edgeByColor[k];
		if (!g.pts.length) return;
		var geo = new THREE.BufferGeometry();
		geo.setAttribute("position", new THREE.Float32BufferAttribute(g.pts, 3));
		var mat = new THREE.LineBasicMaterial({
			color: g.col,
			transparent: true,
			opacity: 0.4,
		});
		var seg = new THREE.LineSegments(geo, mat);
		starmapScene.add(seg);
		starmapEdgeLines.push(seg);
	});
}

// starmapRebuildEdgePositions 在布局更新后重建边几何。
//
// ★ 同时刷新光晕实例：光晕是 InstancedMesh，实例矩阵里**烧进了位置**，
//   节点坐标一变就必须重写矩阵，否则光晕留在旧位置上（看着像“光斑漂移”）。
function starmapRebuildEdgePositions(pos) {
	if (!starmapScene) return;
	starmapBuildEdges(pos);
	starmapRebuildGlowInstances(pos);
}
// ===== BEGIN shared:starmap/layout =====
// 星图力导向布局 —— WebUI 与 GUI 的**唯一事实源**。
//
// ===================================================================
// 这个文件为什么存在（别把它当成“又一份副本”）
// ===================================================================
//
// 下面这三个函数原本在 `internal/plugins/webui/dashboard.js` 与
// `cmd/gui/renderer/app.js` 里各有一份手抄。分叉的代价已经兑现过两次：
//
//   1. WebUI 把 O(n²) 布局换成网格近邻（生产库 3754 块：3.52 亿次配对 →
//      n·k 次），GUI 那份没跟着改 ⇒ 换客户端卡顿原样存在。
//   2. 把空闲帧跳过从 WebUI 抄进 GUI 时抄了 `starmapMorphSet`（WebUI 独有），
//      GUI 实际用 `state.starmapGrown` ⇒ 渲染循环每帧 ReferenceError、星图 0 帧。
//
// 所以这里放**唯一一份**实现，由 `tools/starmap-sync.mjs` 写进两个客户端文件
// 的 `BEGIN/END shared:starmap/layout` 标记块内，并由判据钉住“三处逐字节相同”。
//
// ===================================================================
// 算法
// ===================================================================
//
// 被替换掉的旧实现是 50 轮 `for i / for j=i+1` 全对全斥力：
// 生产库 3754 块 ⇒ 每轮 704 万次配对、50 轮 3.52 亿次。实测（CDP Profiler
// + Electron）单次建图阻塞主线程 1.8–10.9 秒。
//
// 关键观察：原斥力体里本来就有 `if (d < 25)` —— **只对近邻施力**。既然只关心
// 近邻，就没必要算全对全：把空间切成边长 == 斥力半径的格子，每个节点只与
// 「自身格 + 相邻 26 格」（3×3×3）内的点比较即可。
//
// ★ 为何格边长必须恰等于斥力半径：这样「距离 < 25 的点」必然落在相邻 26 格内，
//   **绝不遗漏** —— 这是敢跳过远处配对的唯一依据。格边长一旦小于斥力半径，
//   就会漏掉真实邻居，图会塌成一团。
//
// 复杂度 O(n·k)：均匀分布下每格约 1 点 ⇒ 每轮约 27n 次比较。
// 配合迭代数自适应与收敛早停，3754 节点实测 8365ms → 438ms。

// 自适应迭代数：小图跑满 50 轮（值得精细），大图退到 14 轮。
//
// n=3754 时约 15 轮。旧实现固定 50 轮，且每轮是 O(n²)。
// 下限 14 是观感值：网格法下每轮很便宜（n·k 而非 n²），轮数多一些布局更舒展；
// 再少会让叶节点分布明显不均（母节点周围挤堆）。
function starmapLayoutIterationsFor(n) {
	if (n <= 400) return 50;
	var it = Math.floor(60000 / n);
	if (it < 14) it = 14;
	if (it > 50) it = 50;
	return it;
}

// 早停阈值：取「节点平均间距的 0.2%」。
// 用相对量而不是绝对值，否则同一个阈值在不同规模的图上含义完全不同。
function starmapLayoutMaxMoveThreshold(n, maxR) {
	var perimeter = 2 * Math.PI * maxR;
	var gap = n > 0 ? perimeter / n : maxR;
	var th = gap * 0.002;
	var cap = maxR * 2 * 0.001;
	return th < cap ? th : cap;
}

// starmapLayoutRelax 就地松弛坐标。
//
// 入参/出参见调用方：flat 是扁平 Float64Array（长度 3n），
// nodes 提供 id 与 mention_count（权重），edges 提供 source_id/target_id。
// 用扁平数组而非对象数组，是为了让可行时能直接走结构化克隆交给 Worker。
function starmapLayoutRelax(flat, nodes, edges, maxR) {
	var n = nodes.length;
	if (n === 0) return flat;
	var CUTOFF = 25; // == 斥力半径，见文件头说明
	var BASE_R = 15;
	// 归一化权重（斥力/吸力都按它加权）
	var mn = new Float64Array(n);
	var maxMc = 1,
		minMc = 0;
	for (var a = 0; a < n; a++) {
		var m = (nodes[a] && nodes[a].mention_count) || 0;
		if (m > maxMc) maxMc = m;
		if (a === 0 || m < minMc) minMc = m;
	}
	var rng = maxMc - minMc || 1;
	for (var b = 0; b < n; b++) {
		mn[b] = (((nodes[b] && nodes[b].mention_count) || 0) - minMc) / rng;
	}
	// 边下标（id → 下标对）。下标化是为了内层循环不做字符串比较。
	var idx = {};
	for (var i0 = 0; i0 < n; i0++) idx[nodes[i0].id] = i0;
	var edgeIdx = [];
	for (var e0 = 0; e0 < edges.length; e0++) {
		var s0 = idx[edges[e0].source_id],
			t0 = idx[edges[e0].target_id];
		if (s0 === undefined || t0 === undefined || s0 === t0) continue;
		edgeIdx.push([s0, t0]);
	}
	var grid = new Map();
	function rebuildGrid() {
		grid.clear();
		for (var i = 0; i < n; i++) {
			var key =
				Math.floor(flat[i * 3] / CUTOFF) +
				"|" +
				Math.floor(flat[i * 3 + 1] / CUTOFF) +
				"|" +
				Math.floor(flat[i * 3 + 2] / CUTOFF);
			var bucket = grid.get(key);
			if (!bucket) grid.set(key, (bucket = []));
			bucket.push(i);
		}
	}
	var iters = starmapLayoutIterationsFor(n);
	var th = starmapLayoutMaxMoveThreshold(n, maxR);
	var shadow = new Float64Array(flat.length);
	shadow.set(flat);
	for (var it = 0; it < iters; it++) {
		rebuildGrid();
		// 斥力：只算 3×3×3 邻格内的近邻；每对只处理一次（j > i），
		// 与旧实现 `for j = i+1` 的语义一致，避免重复施力。
		for (var i1 = 0; i1 < n; i1++) {
			var cxi = Math.floor(flat[i1 * 3] / CUTOFF);
			var cyi = Math.floor(flat[i1 * 3 + 1] / CUTOFF);
			var czi = Math.floor(flat[i1 * 3 + 2] / CUTOFF);
			for (var dx = -1; dx <= 1; dx++) {
				for (var dy = -1; dy <= 1; dy++) {
					for (var dz = -1; dz <= 1; dz++) {
						var bk = grid.get(cxi + dx + "|" + (cyi + dy) + "|" + (czi + dz));
						if (!bk) continue;
						for (var bi = 0; bi < bk.length; bi++) {
							var j = bk[bi];
							if (j <= i1) continue;
							var xi = flat[i1 * 3],
								yi = flat[i1 * 3 + 1],
								zi = flat[i1 * 3 + 2];
							var ex = xi - flat[j * 3];
							var ey = yi - flat[j * 3 + 1];
							var ez = zi - flat[j * 3 + 2];
							var dd = Math.sqrt(ex * ex + ey * ey + ez * ez) + 0.1;
							if (dd >= CUTOFF) continue;
							var rf = 0.5 + (mn[i1] + mn[j]) * 0.5;
							var force = (0.06 * rf) / Math.max(dd, 0.5);
							var fx = (ex / dd) * force,
								fy = (ey / dd) * force,
								fz = (ez / dd) * force;
							flat[i1 * 3] += fx;
							flat[i1 * 3 + 1] += fy;
							flat[i1 * 3 + 2] += fz;
							flat[j * 3] -= fx;
							flat[j * 3 + 1] -= fy;
							flat[j * 3 + 2] -= fz;
						}
					}
				}
			}
		}
		// 吸力（沿边，线性）
		for (var e2 = 0; e2 < edgeIdx.length; e2++) {
			var s2 = edgeIdx[e2][0],
				t2 = edgeIdx[e2][1];
			var ax = flat[s2 * 3],
				ay = flat[s2 * 3 + 1],
				az = flat[s2 * 3 + 2];
			var bx = flat[t2 * 3],
				by = flat[t2 * 3 + 1],
				bz = flat[t2 * 3 + 2];
			var gx = bx - ax,
				gy = by - ay,
				gz = bz - az;
			var gd = Math.sqrt(gx * gx + gy * gy + gz * gz) + 0.1;
			if (gd <= 20) continue;
			var af = 0.04 * Math.max(0.3, 1.0 - (mn[s2] + mn[t2]) * 0.3);
			var ux = (gx / gd) * af,
				uy = (gy / gd) * af,
				uz = (gz / gd) * af;
			flat[s2 * 3] += ux;
			flat[s2 * 3 + 1] += uy;
			flat[s2 * 3 + 2] += uz;
			flat[t2 * 3] -= ux;
			flat[t2 * 3 + 1] -= uy;
			flat[t2 * 3 + 2] -= uz;
		}
		// 向心约束 + 早停位移统计（一次遍历同时做完，省一趟内存读）
		var maxA = maxR * 1.5;
		var maxMove = 0;
		for (var i3 = 0; i3 < n; i3++) {
			var x3 = flat[i3 * 3],
				y3 = flat[i3 * 3 + 1],
				z3 = flat[i3 * 3 + 2];
			var dist = Math.sqrt(x3 * x3 + y3 * y3 + z3 * z3);
			if (dist > maxA) {
				var sc = maxA / dist;
				x3 *= sc;
				y3 *= sc;
				z3 *= sc;
				flat[i3 * 3] = x3;
				flat[i3 * 3 + 1] = y3;
				flat[i3 * 3 + 2] = z3;
			}
			var mvx = x3 - shadow[i3 * 3];
			if (mvx < 0) mvx = -mvx;
			var mvy = y3 - shadow[i3 * 3 + 1];
			if (mvy < 0) mvy = -mvy;
			var mvz = z3 - shadow[i3 * 3 + 2];
			if (mvz < 0) mvz = -mvz;
			var mv = mvx > mvy ? mvx : mvy;
			if (mvz > mv) mv = mvz;
			if (mv > maxMove) maxMove = mv;
		}
		shadow.set(flat);
		if (maxMove < th) break; // 收敛：剩余轮次只是重复同一状态
	}
	return flat;
}
// 本块由 tools/starmap-sync.mjs 从 shared/starmap/layout.js 生成 —— 请改共享源，不要直接改这里。
// ===== END shared:starmap/layout =====

// ===== 布局 Worker 调度 =====
//
// starmapLayoutWorkerAvailable 只在浏览器支持且脚本可加载时为真；
// file:// 打开时 Worker 构造会抛错，自动落到串行回退。
function starmapLayoutWorkerAvailable() {
	return !starmapLayoutWorkerBroken && typeof Worker !== "undefined";
}

// starmapRequestLayout 把布局交给 Worker，返回主线程不阻塞。
//
// ★ 为何要 check 消息来回（而不是直接在 onmessage 里 resolve 更简单）：
//   Worker 首次加载脚本有延迟，且同一页面可能连续触发多次布局。
//   用自增 seq 丢弃过期应答，避免「先点开的图被后一次的布局覆盖」。
function starmapRequestLayout(flat, maxR, nodeDegs, onDone) {
	try {
		if (!starmapLayoutWorker) {
			starmapLayoutWorker = new Worker("/static/starmap-layout-worker.js");
			starmapLayoutWorker.onerror = () => {
				// 脚本 404 / 语法错：标记不可用，此后走串行回退。
				// 不抛出去（会让整个 buildChatStarmapGraph 中断、图建不出来）。
				starmapLayoutWorkerBroken = true;
				starmapLayoutWorker = null;
			};
			starmapLayoutWorker.onmessage = (ev) => {
				var m = ev.data || {};
				var cb = starmapLayoutPending[m.seq];
				if (!cb) return;
				delete starmapLayoutPending[m.seq];
				if (m.type === "error") {
					cb(null, m.message);
					return;
				}
				cb(m.positions, null);
			};
		}
		var seq = ++starmapLayoutSeq;
		// 图指纹做种子：同一张图每次打开得到同一布局（可复现）。
		var seed = starmapGraphSeed();
		var nodes = starmapNodes.map((nd) => ({
			id: nd.id,
			mention_count: nd.mention_count || 0,
		}));
		var idx = {};
		for (var i = 0; i < starmapNodes.length; i++) idx[starmapNodes[i].id] = i;
		var edges = [];
		for (var e = 0; e < starmapEdges.length; e++) {
			var s = idx[starmapEdges[e].source_id],
				t = idx[starmapEdges[e].target_id];
			if (s === undefined || t === undefined || s === t) continue;
			edges.push([s, t]);
		}
		starmapLayoutPending[seq] = (positions, err) => {
			if (err || !positions) {
				// Worker 算失败：串行回退，至少图能出来。
				starmapLayoutRelax(flat, starmapNodes, starmapEdges, maxR);
				onDone(flat);
				return;
			}
			onDone(new Float64Array(positions));
		};
		starmapLayoutWorker.postMessage({
			seq: seq,
			n: starmapNodes.length,
			maxR: maxR,
			nodes: nodes,
			edges: edges,
			positions: flat,
			seed: seed,
		});
	} catch (err) {
		starmapLayoutWorkerBroken = true;
		starmapLayoutRelax(flat, starmapNodes, starmapEdges, maxR);
		onDone(flat);
	}
}

// starmapGraphSeed 由图规模派生的稳定种子（同一张图 ⇒ 同一布局）。
function starmapGraphSeed() {
	return (starmapNodes.length * 2654435761 + starmapEdges.length * 40503) >>> 0;
}

// 回合收尾：由 SSE 事件（agent_output final / reset 帧）或 watchdog 驱动。
// POST 结束 ≠ 回合结束：agent 可能还在生成（排队+长生成），提前复位
// chatLoading 会让后续 delta 走全量重建、停止按钮消失、用户误发重复消息。
function endChatTurn() {
	if (!state.chatLoading) return;
	state.chatLoading = false;
	state.chatStage = "";
	if (state._turnWatchdog) {
		clearTimeout(state._turnWatchdog);
		state._turnWatchdog = null;
	}
	var btn = document.getElementById("chat-send-btn");
	if (btn) {
		btn.disabled = false;
		btn.textContent = __("发送", "Send");
	}
	var sb = document.getElementById("chat-stop-btn");
	if (sb) sb.style.display = "none";
	rerenderChat(true);
}

// ★★ 用量徐标渲染（2026-10-05 从 connectSSE() 内提到顶层）
//
// 此前它定义在 connectSSE() 内部（缩进一个 tab），而 starmapFetchBlock 的
// kernel 分支在 renderAll 里调用它 —— 那是**顶层**调用，此时 connectSSE
// 早已返回 ⇒ renderChatUsage 是 undefined ⇒
//
//	Uncaught (in promise) ReferenceError: renderChatUsage is not defined
//	    at starmapFetchBlock (dashboard.js)
//	    at async Promise.all (index 2)
//	    at async renderAll (dashboard.js)
//
// 而 renderAll 里 `await Promise.all(needs.map(starmapFetchBlock))`
// **没有 try/catch**，且 TABS 的 overview / plugins / kernel 三个页签都含
// "kernel" ⇒ 任一页触发它，Promise.all 整体 reject，后面的 spec.render
// 根本不执行 ⇒ 整页白屏（生产实测）。
//
// 教训与本仓其它「静默失效」同型：**声明位置就是契约**。
// 判据见 dashboard_js_scope_test.go（node 真跑 + 缩进静态检查）。
// 用量徐标渲染。
//
// 口径纪律（与内核 usageLedger 同一套，不能各写一套）：
//   · cache_hit_rate **可能缺席** —— 那是「没有任何调用报过缓存细节」，
//     必须显示「—」而不是 0%。把「不知道」画成 0% 会让人去优化一个
//     本来就没开的功能；
//   · 缺席与「命中率为 0」是两回事，后者是有数据、真的一分没命中。
function renderChatUsage(u, single, source) {
	var el = document.getElementById("chat-usage");
	if (!el) return;
	if (!u || !u.total) {
		el.style.display = "none";
		return;
	}
	function compact(n) {
		n = n || 0;
		if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
		if (n >= 1000) return (n / 1000).toFixed(1) + "k";
		return String(n);
	}
	var hasRate = typeof u.cache_hit_rate === "number";
	el.textContent =
		compact(u.total) +
		" tok · " +
		(hasRate
			? __("缓存 ", "cache ") + Math.round(u.cache_hit_rate * 100) + "%"
			: __("缓存 —", "cache —"));
	var tip = [
		source || "",
		__("调用 ", "calls ") + (u.calls || 0),
		"prompt " + (u.prompt || 0),
		"completion " + (u.completion || 0),
		"cache_read " + (u.cache_read || 0),
		"cache_miss " + (u.cache_miss || 0),
	];
	tip.push(
		hasRate
			? __("命中率 ", "hit rate ") + (u.cache_hit_rate * 100).toFixed(1) + "%"
			: __(
					"命中率未知（上游未报缓存细节）",
					"hit rate unknown (upstream reported no cache)",
				),
	);
	if (single && single.total) {
		tip.push(
			__("本次 ", "last call ") +
				"prompt " +
				(single.prompt || 0) +
				" / completion " +
				(single.completion || 0),
		);
	}
	el.title = tip.filter(Boolean).join("\n");
	el.style.display = "";
}

// 回合看门狗：POST 已 abort 且 SSE 迟迟无终帧时兕底收尾（连接不稳/事件丢失），
// 提示用户回复可能已生成、可刷新查看历史。避免回合永久卡在 loading。
function armTurnWatchdog() {
	if (state._turnWatchdog) clearTimeout(state._turnWatchdog);
	state._turnWatchdog = setTimeout(() => {
		state._turnWatchdog = null;
		if (state.chatLoading) {
			endChatTurn();
			toast(
				__(
					"长时间未收到回复，连接可能不稳定；回复可能已生成，可刷新页面查看",
					"No reply received for a long time; the reply may have been generated, refresh to check",
				),
				true,
			);
		}
	}, 120000);
}

// 上传文件并注入 agent：multipart POST /api/v1/chat/file。
// 服务端落盘 uploads/ 后注入「[用户发送了文件: 名字 (大小)] 已保存到 <路径>」；
// 回复与普通消息一样走 SSE 流式渲染。可选附言从输入框读取。
// 拖拽文件到聊天区即发送（可选：先在输入框写附言）
function initChatDragDrop() {
	var panel = document.getElementById("chat-panel-chat");
	if (!panel || panel.__dnd) return;
	panel.__dnd = true;
	panel.addEventListener("dragover", (e) => {
		e.preventDefault();
		panel.style.outline = "2px dashed var(--accent, #4a90d9)";
	});
	panel.addEventListener("dragleave", () => {
		panel.style.outline = "";
	});
	panel.addEventListener("drop", (e) => {
		e.preventDefault();
		panel.style.outline = "";
		if (e.dataTransfer.files && e.dataTransfer.files.length) {
			sendChatFile(e.dataTransfer.files[0]);
		}
	});
	// 粘贴截图/复制的文件直接发送
	document.addEventListener("paste", (e) => {
		var chatVisible =
			document.getElementById("chat-panel-chat") &&
			document.getElementById("chat-input");
		if (!chatVisible) return;
		var items = e.clipboardData && e.clipboardData.files;
		if (items && items.length && document.activeElement !== null) {
			sendChatFile(items[0]);
		}
	});
}

async function sendChatFile(fileObj, extraText) {
	if (!fileObj || state.chatLoading) return;
	var inp = document.getElementById("chat-input");
	var message = (extraText || inp.value || "").trim();
	if (inp) inp.value = "";
	var fd = new FormData();
	fd.append("file", fileObj);
	if (message) fd.append("message", message);
	state.messages.push({
		role: "user",
		content: message,
		attachment: {
			// 与服务端的 attType 判定保持一致（image/audio/file），
			// 否则乐观渲染的卡片会在 SSE 回流后变成另一种样式。
			type: /^image\//.test(fileObj.type)
				? "image"
				: /^audio\//.test(fileObj.type)
					? "audio"
					: "file",
			url: URL.createObjectURL(fileObj),
			name: fileObj.name,
			size: fileObj.size,
		},
	});
	rerenderChat(true);
	state.chatLoading = true;
	state.chatStage = __("等待AI回复...", "Waiting for AI...");
	rerenderChat(true);
	try {
		await api("/chat/file", { method: "POST", body: fd, rawBody: true });
		// 回复经 SSE 流式到达，这里无需处理响应体
	} catch (e) {
		toast(
			__("文件发送失败：" + e.message, "File send failed: " + e.message),
			true,
		);
		state.chatLoading = false;
		endChatTurn();
	}
}

async function sendChat() {
	var inp = document.getElementById("chat-input");
	var btn = document.getElementById("chat-send-btn");
	var stopBtn = document.getElementById("chat-stop-btn");
	var text = inp.value.trim();
	if (!text || state.chatLoading) return;
	state.chatStick = true;
	state.chatFinalIdx = -1;
	state.messages.push({ role: "user", content: text });
	inp.value = "";
	rerenderChat(true);
	state.chatLoading = true;
	state.chatStage = __("等待AI回复...", "Waiting for AI...");
	btn.disabled = true;
	btn.textContent = "";
	if (stopBtn) stopBtn.style.display = ""; // 生成期间可停止
	rerenderChat(true);
	// 触发式 POST：短超时仅确认受理；回复靠 SSE 流式渲染（对齐 GUI 行为）。
	try {
		// ★★ 受理超时（2026-10-17 内测故障修复）
		//
		// 原来写死 15s。实测（用户日志）agent 正常任务就要 **52.5 秒**
		// （22 个工具：skill_info/memory_recall/cmd_run×6/terminal_create…），
		// 于是 15s 一到前端就 abort：
		//
		//   20:58:36  POST /chat 开始，agent 开始干活
		//   20:58:52  ★ 15s abort → 服务端记 status=504 (15.89s)
		//   20:59:29  agent 仍然正常跑完 52.5 秒并返回 —— 但连接已断，
		//             回复送不回去，用户只看到「已取消（用户停止）」
		//
		// 危害不只是"慢"：它让用户以为自己误触了停止，或者以为系统坏了。
		//
		// 现在：不再有固定超时。这个 POST 是"触发式"的 —— 真正渲染靠 SSE
		// （见下方 finally 的 armTurnWatchdog），所以**没必要**在这里抢时间。
		// 只保留一个远大于任何正常任务的上限（10 分钟），用于兜住
		// "连 TCP 都建不起来"这类真故障，避免请求永远挂着。
		var ctrl = new AbortController();
		var ackTimer = setTimeout(() => {
			ctrl.abort();
		}, 600000);
		var r = null;
		try {
			r = await api("/chat", {
				method: "POST",
				body: JSON.stringify({ message: text }),
				signal: ctrl.signal,
			});
		} catch (ackErr) {
			// 同步超时/失败：不阻塞 UI，等 SSE 兑底；明确提示"可能已发送"
			console.warn("[sendChat] trigger failed: " + ackErr.message);
			// 措辞要点：这不是"失败"，而是"受理确认没等到"。
			// agent 很可能正在正常干活（实测 52 秒的任务很常见），
			// 所以**不要**让用户以为出了问题而重发 —— 重发会造成两条任务。
			toast(
				__(
					"已发出，AI 正在处理（复杂任务可能要一两分钟）。请等待回复，不要重复发送。",
					"Sent — the AI is working (complex tasks may take a minute or two). Please wait; do not resend.",
				),
				false,
			);
			r = null;
		} finally {
			clearTimeout(ackTimer);
		}
		var last = state.messages[state.messages.length - 1];
		if (r && r.response) {
			if (last && last.role === "assistant" && last._streaming) {
				last.content = r.response || __("(无响应)", "(no response)");
				last._grow = true;
				if (!last.reasoning_content) {
					last.reasoning_content = r.reasoning_content || "";
				}
				last._final = true;
				delete last._streaming;
			} else {
				state.messages.push({
					role: "assistant",
					content: r.response || __("(无响应)", "(no response)"),
					reasoning_content: r.reasoning_content,
					tool_calls:
						last && last.role === "assistant" && last.tool_calls
							? last.tool_calls
							: [],
					_final: true,
					_grow: true,
				});
			}
			state.chatFinalIdx = state.messages.length - 1;
		}
		rerenderChat(true);
	} catch (e) {
		// 真实错误（非受理超时）：展示错误信息
		if (!String(e.message || "").includes("aborted")) {
			state.messages.push({
				role: "assistant",
				content: __("错误: ", "Error: ") + e.message,
				_final: true,
			});
			rerenderChat(true);
			toast(__("请求失败: ", "Request failed: ") + e.message, true);
		}
	} finally {
		if (r && r.response) {
			// 同步兜底已拿到完整回复：回合结束
			endChatTurn();
		} else {
			// 触发式受理（POST 已 abort/失败）：回合仍打开，等 SSE 流式渲染；
			// 由 agent_output final / reset 帧 / watchdog 收尾
			armTurnWatchdog();
		}
	}
}

// 停止生成 / 发送中断消息。核心拦截语义：有 LLM 在跑则取消当前
// 请求并以 [中断消息] 重启轮次；无则在跑则作为普通消息处理。
async function interruptChat() {
	try {
		await api("/chat/interrupt", {
			method: "POST",
			body: JSON.stringify({}),
		});
		toast(__("已发送中断信号", "Interrupt signal sent"));
	} catch (e) {
		toast(__("中断失败: ", "Interrupt failed: ") + e.message, true);
	}
}

async function queryMemoryChat() {
	var q = document.getElementById("mem-query")?.value;
	var r = document.getElementById("mem-result-chat");
	if (!r || !q) return;
	r.innerHTML = '<div class="loading"></div>';
	try {
		var data = await api("/memory?q=" + encodeURIComponent(q) + "&depth=2");
		r.innerHTML =
			'<pre style="font-size:11px">' +
			escHtml(JSON.stringify(data, null, 2)) +
			"</pre>";
	} catch (e) {
		r.innerHTML =
			'<p style="color:#fca5a5">' +
			__("查询失败: ", "Query failed: ") +
			escHtml(e.message) +
			"</p>";
	}
}

async function queryMemoryContext() {
	var q = document.getElementById("ctx-query")?.value;
	var r = document.getElementById("ctx-result");
	if (!r) return;
	r.innerHTML = '<div class="loading"></div>';
	try {
		var data = await api("/memory/context?q=" + encodeURIComponent(q || ""));
		var ctx = data?.context || __("无上下文", "No context");
		var summary = data?.summary || "";
		var entities = data?.entities || [];
		var docs = data?.docs || [];
		var tk = data?.token_estimate || 0;
		var html = '<div style="font-size:11px">';
		// ★ 召回是 L2+L3 **双路**（2026-10-27）：这里必须两路都显示。
		// 此前只显示 L3 图记忆，而 L3 有 72h 冷化延迟 —— 冷化没跑起来时
		// 面板会一直显示"无上下文"，让人以为召回彻底坏了（实际 L2 一直有命中）。
		if (docs.length) {
			html +=
				'<div class="kv-row" style="align-items:flex-start"><span class="key">' +
				__("L2 冷上下文", "L2 cold context") +
				'</span><span class="val">' +
				docs
					.map((d) => {
						var when = String(d.when || "").slice(5, 16);
						var score = Number(d.score || 0).toFixed(2);
						return (
							'<div style="margin-bottom:4px">' +
							'<span style="opacity:.7">' + escHtml(when) + " · " + score + "</span> " +
							escHtml(d.text || "") +
							"</div>"
						);
					})
					.join("") +
				"</span></div>";
		}
		if (entities.length) {
			html +=
				'<div class="kv-row"><span class="key">' +
				__("L3 图记忆实体", "L3 graph entities") +
				'</span><span class="val">' +
				entities.length +
				"</span></div>";
		}
		if (summary)
			html +=
				'<div class="kv-row"><span class="key">' +
				__("摘要", "Summary") +
				'</span><span class="val">' +
				escHtml(summary) +
				"</span></div>";
		html +=
			'<div class="kv-row"><span class="key">Token ' +
			__("预估", "Estimate") +
			'</span><span class="val">' +
			tk +
			"</span></div>";
		if (entities.length) {
			html +=
				'<div class="kv-row"><span class="key">' +
				__("实体", "Entities") +
				'</span><span class="val">' +
				entities.map((e) => escHtml(e.name || e.id || "")).join(", ") +
				"</span></div>";
		}
		html +=
			'<pre style="font-size:11px;margin-top:8px">' +
			escHtml(ctx) +
			"</pre></div>";
		r.innerHTML = html;
	} catch (e) {
		r.innerHTML =
			'<p style="color:#fca5a5">' +
			__("获取失败: ", "Get failed: ") +
			escHtml(e.message) +
			"</p>";
	}
}

// 知识库面板：搜索 / 创建 / 删除 / 刷新计数。
//
// 此前三处问题：搜索把裸 JSON 直接 stringify 丢进 <pre>（用户看到一坨
// 机器码）；创建后不刷新计数（1644 行读的是 state.kernel 快照，创建
// 完仍是旧值）；没有任何删除入口，也没有媒体上传。
function knowEsc(v) {
	return String(v == null ? "" : v)
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;");
}

async function refreshKnowledgeCount() {
	try {
		var d = await api("/knowledge");
		var el = document.getElementById("know-count");
		if (el && d && d.names) el.textContent = d.names.length;
		// 稠密路（多模态）状态提示
		var hint = document.getElementById("know-media-hint");
		if (hint && d && d.dense) {
			if (d.dense.enabled) {
				hint.textContent =
					__("多模态已就绪 ", "Multimodal ready ") +
					(d.dense.ready || 0) +
					"/" +
					((d.dense.ready || 0) + (d.dense.stale || 0));
			} else {
				hint.textContent = __(
					"未接入多模态：图片只记录不参与召回",
					"Multimodal off: images recorded but not searchable",
				);
			}
		}
	} catch (e) {
		/* 计数刷新失败不该打断用户操作 */
	}
}

function renderKnowledgeResults(views, q) {
	if (!views || !views.length) {
		return (
			'<p style="opacity:.7;font-size:12px">' +
			__("未找到相关知识", "No matching knowledge") +
			"</p>"
		);
	}
	var html = "";
	views.forEach((v) => {
		html +=
			'<div class="know-item" style="padding:6px 0;border-bottom:1px solid var(--border-color)">';
		html += '<div style="display:flex;gap:6px;align-items:baseline">';
		html +=
			'<strong style="font-size:12px;flex:1;word-break:break-all">' +
			knowEsc(v.name) +
			"</strong>";
		if (v.size) {
			html += '<span style="font-size:10px;opacity:.6">' + v.size + " B</span>";
		}
		html +=
			'<button class="btn btn-sm" style="font-size:10px" onclick="deleteKnowledge(' +
			JSON.stringify(v.name).replace(/"/g, "&quot;") +
			')">' +
			__("删除", "Delete") +
			"</button>";
		html += "</div>";
		if (v.media && v.media.length) {
			html += '<div style="font-size:10px;opacity:.7;margin-top:2px">';
			v.media.forEach((m) => {
				var kind = m.kind || "file";
				html +=
					'<span style="margin-right:6px">[' +
					knowEsc(kind) +
					"] " +
					knowEsc(m.digest.slice(0, 8)) +
					"…</span>";
			});
			html += "</div>";
		}
		if (v.preview) {
			html +=
				'<div style="font-size:11px;opacity:.8;margin-top:3px;white-space:pre-wrap;max-height:80px;overflow:auto">' +
				knowEsc(v.preview) +
				"</div>";
		}
		html += "</div>";
	});
	return html;
}

// 知识库分类树浏览。
//
// 数据来自 /knowledge/tree（只读树接口）。当前分类存 state，
// 搜索时自动带上 —— 这样"先定位分类再检索"这个更准的用法在 UI 上
// 是一步的事，而不是要求用户手打分类名。
var _knowCat = ""; // 当前分类的完整 path；"" = 全库

function renderKnowTree(node, depth) {
	var html = "";
	// 本节点的条目
	(node.items || []).forEach((it) => {
		html +=
			'<div style="padding-left:' +
			(depth * 12 + 4) +
			'px;padding-top:1px;padding-bottom:1px">' +
			'<span style="opacity:.7">▸</span> ' +
			'<span style="word-break:break-all">' +
			knowEsc(it.name) +
			"</span>" +
			(it.size
				? ' <span style="opacity:.5;font-size:10px">' + it.size + "B</span>"
				: "") +
			(it.media && it.media.length
				? ' <span style="opacity:.6;font-size:10px">[' +
					it.media.length +
					__(" 媒体", " media") +
					"]</span>"
				: "") +
			"</div>";
	});
	// 子分类
	(node.children || []).forEach((c) => {
		var isCur = _knowCat === c.path;
		var mark = isCur ? "● " : "";
		html +=
			'<div style="padding-left:' +
			depth * 12 +
			'px"><span onclick="selectKnowledgeCategory(\'' +
			knowEsc(c.path).replace(/'/g, "&#39;") +
			'\')" style="cursor:pointer;user-select:none">' +
			mark +
			knowEsc(c.name) +
			' <span style="opacity:.55;font-size:10px">' +
			c.item_count +
			"/" +
			c.total_count +
			"</span></span></div>";
		html += renderKnowTree(c, depth + 1);
	});
	return html;
}

async function loadKnowledgeTree() {
	var box = document.getElementById("know-tree");
	if (!box) return;
	box.innerHTML = '<div class="loading"></div>';
	try {
		var d = await api("/knowledge/tree?items=1");
		if (!d || !d.tree) {
			box.innerHTML =
				'<p style="opacity:.6;font-size:11px">' +
				__("暂无分类", "No categories yet") +
				"</p>";
			return;
		}
		box.innerHTML = renderKnowTree(d.tree, 0);
	} catch (e) {
		box.innerHTML =
			'<p style="color:#fca5a5;font-size:11px">' +
			__("加载分类失败: ", "Load categories failed: ") +
			knowEsc(e.message) +
			"</p>";
	}
}

function selectKnowledgeCategory(path) {
	_knowCat = path || "";
	var lbl = document.getElementById("know-cat-scope");
	if (lbl) {
		lbl.textContent = _knowCat
			? __("范围：", "scope: ") + _knowCat
			: __("范围：全库", "scope: all");
	}
	loadKnowledgeTree();
	// 立刻按新范围搜一次，省一次点击
	var q = document.getElementById("know-query")?.value;
	if (q) searchKnowledgeChat();
}

function clearKnowledgeCategory() {
	selectKnowledgeCategory("");
}

async function searchKnowledgeChat() {
	var q = document.getElementById("know-query")?.value;
	var cat = _knowCat || "";
	var r = document.getElementById("know-result-chat");
	if (!r) return;
	if (!q) {
		r.innerHTML =
			'<p style="opacity:.7;font-size:12px">' +
			__("请输入查询关键词", "Enter a keyword") +
			"</p>";
		return;
	}
	r.innerHTML = '<div class="loading"></div>';
	try {
		var url = "/knowledge?q=" + encodeURIComponent(q);
		if (cat) url += "&category=" + encodeURIComponent(cat);
		var data = await api(url);
		r.innerHTML = renderKnowledgeResults(data && data.results, q);
	} catch (e) {
		r.innerHTML =
			'<p style="color:#fca5a5">' +
			__("搜索失败: ", "Search failed: ") +
			knowEsc(e.message) +
			"</p>";
	}
}

async function deleteKnowledge(name) {
	if (
		!confirm(
			__("确定删除知识「", 'Delete knowledge "') +
				name +
				__("」？此操作不可撤销。", '"? This cannot be undone.'),
		)
	) {
		return;
	}
	try {
		await api("/knowledge?name=" + encodeURIComponent(name), {
			method: "DELETE",
		});
		toast(__("知识「", 'Knowledge "') + name + __("」已删除", '" deleted'));
		refreshKnowledgeCount();
		searchKnowledgeChat();
	} catch (e) {
		toast(__("删除失败: ", "Delete failed: ") + e.message, true);
	}
}

async function createKnowledgeChat() {
	var name = document.getElementById("know-name")?.value;
	var content = document.getElementById("know-content")?.value;
	var fileInput = document.getElementById("know-media");
	var files = fileInput && fileInput.files ? fileInput.files : null;
	if (!name) {
		toast(__("名称不能为空", "Name is required"), true);
		return;
	}
	if ((!content || !content.trim()) && (!files || !files.length)) {
		toast(__("内容与媒体至少要有一项", "Content or media is required"), true);
		return;
	}
	try {
		var r;
		if (files && files.length) {
			// 有文件走 multipart：服务端按**探测到的真实类型**分流，
			// 图片/音视频入媒体库并按 digest 挂到条目上，文本存正文。
			var fd = new FormData();
			fd.append("name", name);
			if (content) fd.append("content", content);
			for (var i = 0; i < files.length; i++) fd.append("file", files[i]);
			r = await api("/knowledge", { method: "POST", body: fd });
		} else {
			r = await api("/knowledge", {
				method: "POST",
				body: JSON.stringify({ name: name, content: content }),
			});
		}
		var msg = __("知识「", 'Knowledge "') + name + __("」已创建", '" created');
		if (r && r.media)
			msg += __("，含 ", " with ") + r.media + __(" 个媒体", " media item(s)");
		if (r && r.rejected && r.rejected.length) {
			msg += __("；", "; ") + r.rejected.length + __(" 项被跳过", " skipped");
		}
		toast(msg);
		document.getElementById("know-name").value = "";
		document.getElementById("know-content").value = "";
		if (fileInput) fileInput.value = "";
		refreshKnowledgeCount();
	} catch (e) {
		toast(__("创建失败: ", "Create failed: ") + e.message, true);
	}
}

function switchChatPanel(tab, el) {
	// 切到知识面板时拉实时计数：面板里的数字来自 state.kernel 快照，
	// 而知识条目会经工具/上传增删，快照不会自己变（实测创建后仍显示 "-"）。
	if (tab === "knowledge") {
		refreshKnowledgeCount();
		var lbl = document.getElementById("know-cat-scope");
		if (lbl && !lbl.textContent) {
			lbl.textContent = _knowCat
				? __("范围：", "scope: ") + _knowCat
				: __("范围：全库", "scope: all");
		}
		loadKnowledgeTree();
	}
	var panels = {
		chat: document.getElementById("chat-panel-chat"),
		starmap: document.getElementById("chat-panel-starmap"),
		terminal: document.getElementById("chat-panel-terminal"),
		cmd: document.getElementById("chat-panel-cmd"),
		memory: document.getElementById("chat-panel-memory"),
		context: document.getElementById("chat-panel-context"),
		knowledge: document.getElementById("chat-panel-knowledge"),
	};
	Object.keys(panels).forEach((k) => {
		var p = panels[k];
		if (p) p.classList.toggle("active", k === tab);
	});
	if (el) {
		var parent = el.parentElement;
		if (parent) {
			Array.from(parent.children).forEach((ch) => {
				ch.classList.remove("active");
			});
			el.classList.add("active");
		}
	}
	if (tab === "starmap") {
		if (!state.starmapInit) {
			state.starmapLoading = false;
			renderChatStarmap();
		} else if (starmapRen) {
			var cont = document.getElementById("sm-container-chat");
			if (cont) {
				var rect = cont.getBoundingClientRect();
				if (rect.width > 0)
					starmapRen.setSize(rect.width, Math.max(rect.height, 250));
				if (!cont.contains(starmapRen.domElement))
					cont.appendChild(starmapRen.domElement);
			}
		}
	}
	if (tab === "chat") {
		state.chatStick = true;
		var msgsEl = document.getElementById("chat-msgs");
		if (msgsEl) {
			try {
				msgsEl.scrollTo({ top: msgsEl.scrollHeight, behavior: "smooth" });
			} catch (e) {
				msgsEl.scrollTop = msgsEl.scrollHeight;
			}
		}
	}
	if (tab === "context") queryMemoryContext();
}

// 首屏分段加载条数：只拉最新 N 条，向上滚动触顶时再拉更早的。
var CHAT_PAGE_SIZE = 40;
async function loadChatHistory() {
	try {
		var data = await api("/chat/history?limit=" + CHAT_PAGE_SIZE);
		if (data && data.messages) {
			state.messages = data.messages;
			state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
			state.chatTotal =
				typeof data.total === "number" ? data.total : data.messages.length;
			state.chatHasMore = !!data.has_more;
			state.chatLastSeq = historyLastSeq(data);
		}
	} catch (e) {}
}

// historyLastSeq 从一次 /chat/history 响应里取出「已见到的最大 seq」：
// 优先用服务端给的 last_seq，缺了就取消息里的最大值。
function historyLastSeq(data) {
	if (!data) return 0;
	if (typeof data.last_seq === "number") return data.last_seq;
	var mx = 0;
	(data.messages || []).forEach((m) => {
		if (m && m.seq > mx) mx = m.seq;
	});
	return mx;
}

// applyServerMessages 把服务端消息并进 state.messages，按 seq 对账：
//   - 同 seq 已存在 → 原地替换（工具调用/最终文本是原地更新）；
//   - 不存在 → 追加（若末尾是无 seq 的乐观消息且 role+content 一致，则替换它，
//     避免"自己刚发的那条"重复成两条）。
// tailOnly=true 时只做原地更新与"比本地新才追加"，不把尾探测当成新消息。
// @returns {boolean} 是否真的改动了 state.messages
function applyServerMessages(list, tailOnly) {
	if (!list || !list.length) return false;
	var msgs = state.messages;
	var changed = false;
	function carry(prev, sm) {
		if (prev && prev._final) sm._final = true;
		if (prev && prev._grow) sm._grow = true;
		return sm;
	}
	list.forEach((sm) => {
		if (!sm) return;
		var seq = sm.seq;
		var found = -1;
		for (var j = msgs.length - 1; j >= 0 && j >= msgs.length - 12; j--) {
			if (seq && msgs[j] && msgs[j].seq === seq) {
				found = j;
				break;
			}
		}
		if (found >= 0) {
			if (JSON.stringify(msgs[found]) !== JSON.stringify(sm)) {
				msgs[found] = carry(msgs[found], sm);
				changed = true;
			}
			return;
		}
		if (tailOnly) {
			var lastS = msgs.length ? msgs[msgs.length - 1].seq : 0;
			if (seq && (!lastS || seq > lastS)) {
				msgs.push(sm);
				changed = true;
			}
			return;
		}
		if (msgs.length) {
			var last = msgs[msgs.length - 1];
			if (
				!last.seq &&
				(last.role || "") === (sm.role || "") &&
				(last.content || "") === (sm.content || "")
			) {
				msgs[msgs.length - 1] = carry(last, sm);
				changed = true;
				return;
			}
		}
		msgs.push(sm);
		changed = true;
	});
	list.forEach((sm) => {
		if (sm && sm.seq > (state.chatLastSeq || 0)) state.chatLastSeq = sm.seq;
	});
	if (changed) rerenderChat();
	return changed;
}

// pollChatIncremental 轮询「自上次以来新增了什么」。
//
// 这就是「暴露数据查询 api，前端轮询后 patch 视图」那条路：after=游标
// 只拿增量，再单独探一次尾部做原地更新（工具调用/最终文本是原地改的，
// 不会产生新 seq，只靠 after 拿不到）。视图更新走 commitChatList 的
// keyed 对账，未变消息节点一个字节都不动 —— 闪烁由此消失。
function pollChatIncremental() {
	var after = state.chatLastSeq || 0;
	if (!after) {
		// 还没建立游标（首次 / 本地为空）：退回一次性全量，交给已有一致性逻辑
		return api("/chat/history?limit=" + CHAT_PAGE_SIZE).then((data) => {
			state.chatLastSeq = historyLastSeq(data);
			return mergeChatFromHistory(data);
		});
	}
	return api("/chat/history?after=" + after)
		.then((data) => {
			if (!data) return;
			state.chatLastSeq = historyLastSeq(data) || after;
			applyServerMessages(data.messages || [], false);
			return api("/chat/history?limit=1");
		})
		.then((tail) => {
			if (tail && tail.messages && tail.messages.length) {
				applyServerMessages(tail.messages.slice(-1), true);
			}
		});
}

// loadOlderChat 向上翻页：拉 offset 之前的一页，前置到 messages 头部。
// 保持滚动位置（插入前后 scrollHeight 差值补偿），避免视口跳动。
var _loadingOlder = false;
async function loadOlderChat() {
	if (_loadingOlder || !state.chatHasMore) return;
	_loadingOlder = true;
	var msgsEl = document.getElementById("chat-msgs");
	var prevH = msgsEl ? msgsEl.scrollHeight : 0;
	var prevTop = msgsEl ? msgsEl.scrollTop : 0;
	try {
		var before = state.chatOffset || 0;
		if (before <= 0) {
			state.chatHasMore = false;
			return;
		}
		var data = await api(
			"/chat/history?limit=" + CHAT_PAGE_SIZE + "&before=" + before,
		);
		if (data && data.messages && data.messages.length) {
			state.messages = data.messages.concat(state.messages);
			state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
			state.chatHasMore = !!data.has_more;
			state.chatStick = false; // 向上翻页时不自动粘底
			renderChat();
			if (msgsEl) {
				// 补偿新增内容高度，保持用户视觉位置不变
				msgsEl.scrollTop = prevTop + (msgsEl.scrollHeight - prevH);
			}
		} else {
			state.chatHasMore = false;
		}
	} catch (e) {
	} finally {
		_loadingOlder = false;
	}
}

// syncChatFromHistory 增量同步：先做一次极轻的"尾巴探测"，只有尾巴变了才拉整页。
//
// 原先每 30s（以及每次 SSE 报错）都直接拉一页 40 条：本地实测 180KB、
// 生产消息更大时可达 ~1MB —— 这是"每次都在发完整聊天记录"的观感来源。
// 探测只需 1 条（约几 KB），尾巴一致就直接跳过。
var _syncingChat = false;
// syncChatFromHistory 保留旧名（多处调用点）：内部改走游标增量轮询。
function syncChatFromHistory() {
	if (_syncingChat) return Promise.resolve();
	_syncingChat = true;
	return pollChatIncremental()
		.catch(() => {})
		.then(() => {
			_syncingChat = false;
		});
}

// mergeChatFromHistory 把服务端的一页历史并进本地：只追加新消息，不重建已有节点。
// 关键约束：**绝不**用更短的服务端页替换更长的本地列表（那会让用户翻上来的旧页
// 凭空消失、视口跳回顶部）。
function mergeChatFromHistory(data) {
	if (!data || !data.messages || data.messages.length === 0) return;
	var serverMsgs = data.messages;
	var localMsgs = state.messages;
	// 首次加载（空列表）→ 全量赋值
	if (localMsgs.length === 0) {
		state.messages = serverMsgs;
		state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
		state.chatHasMore = !!data.has_more;
		rerenderChat(true);
		return;
	}
	// 以「本地末尾 vs 服务端末尾」做依据。分段拉取只会回传最新页，
	// 本地已向上翻页加载了更早内容，长度上本地可能 ≥ 服务端。
	var lastLocal = localMsgs[localMsgs.length - 1];
	var lastServer = serverMsgs[serverMsgs.length - 1];
	var localContent = lastLocal.content || lastLocal.Content || "";
	var serverContent = lastServer.content || lastServer.Content || "";
	// 服务端末尾和本地末尾内容相同 → 无新增（可能裁剪过的轻量项因不含 tool_calls args/result，
	// 依赖 content+role+time 足以识别）
	if (localContent === serverContent) {
		// 检查是否本地末尾被服务端改写（如 stage 插件改写最终文本）
		if (
			lastServer.role === "assistant" &&
			localContent !== serverContent &&
			serverContent
		) {
			lastLocal.content = serverContent;
			if (lastServer.ReasoningContent)
				lastLocal.reasoning_content = lastServer.ReasoningContent;
			var msgsEl0 = document.getElementById("chat-msgs");
			if (msgsEl0 && msgsEl0.lastElementChild) {
				var textEl0 =
					msgsEl0.lastElementChild.querySelector(".msg-bubble .text");
				if (textEl0) textEl0.innerHTML = renderMd(serverContent);
			}
		}
		return;
	}
	// 有新增消息：服务端末尾与本地末尾不同 → 找出本地新增的差异，追加到 DOM。
	// 服务端只回传最新页，我们需在 localMsgs 末尾取交集起点（本地末尾在服务端页的位置）
	// 简化：取服务端末尾3条与本地末尾3条比对，找到重含点的下标；若找不到全量替换最新页。
	var overlap = -1;
	for (
		var k = Math.min(3, serverMsgs.length - 1, localMsgs.length - 1);
		k >= 1;
		k--
	) {
		var sMsg = serverMsgs[serverMsgs.length - 1 - k];
		var lMsg = localMsgs[localMsgs.length - 1 - k];
		if (
			lMsg &&
			(lMsg.content || "") === (sMsg.content || "") &&
			lMsg.role === sMsg.role
		) {
			overlap = k;
			break;
		}
	}
	var newMsgs;
	if (overlap >= 0) {
		// 本地末尾 overlap 条在服务端页里，之后的部分是新增
		newMsgs = serverMsgs.slice(serverMsgs.length - overlap);
		if (newMsgs.length === 0) return; // 无新增（内容改写走上面的分支）
	} else {
		// 找不到重合点：**不能**直接拿服务端页覆盖本地。
		// 服务端只回一页，本地翻上来的旧页更长；覆盖会同时造成两个后果：
		// 用户翻过的旧消息凭空消失、容器变矮后视口被夹回顶部。
		// 只有在服务端页不短于本地时才整体替换（那种情况下不丢内容）。
		if (serverMsgs.length >= localMsgs.length) {
			state.messages = serverMsgs;
			rerenderChat(true);
		}
		return;
	}
	var msgsEl = document.getElementById("chat-msgs");
	if (msgsEl) {
		var aiAvatar = '<img src="/mascot.webp" alt="小宅">';
		var userAvatar =
			'<svg viewBox="0 0 24 24" style="width:16px;height:16px" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>';
		newMsgs.forEach((m) => {
			var role = m.role || m.Role || "user";
			var c = m.content || m.Content || "";
			if (role === "assistant") c = renderMd(c);
			else c = escHtml(c);
			var isChan = !!(m.source && m.source !== "webui");
			var bubble = c
				? '<div class="msg-bubble"><div class="text">' + c + "</div></div>"
				: '<div class="msg-bubble"></div>';
			if (role === "system") {
				msgsEl.insertAdjacentHTML(
					"beforeend",
					'<div class="msg msg-system"><div class="msg-bubble">' +
						c +
						"</div></div>",
				);
			} else if (isChan) {
				msgsEl.insertAdjacentHTML(
					"beforeend",
					'<div class="msg msg-channel"><div class="msg-avatar chan-avatar" style="background:#888">' +
						(m.source || "?")[0].toUpperCase() +
						'</div><div class="msg-content"><div class="msg-chan-name">' +
						escHtml(m.source) +
						"</div>" +
						bubble +
						"</div></div>",
				);
			} else {
				msgsEl.insertAdjacentHTML(
					"beforeend",
					'<div class="msg msg-' +
						role +
						'"><div class="msg-avatar">' +
						(role === "user" ? userAvatar : aiAvatar) +
						'</div><div class="msg-content">' +
						bubble +
						"</div></div>",
				);
			}
		});
		// 删除流式占位符（同步完成，下一次 SSE 会重建）
		var streamingPh = msgsEl.querySelector(".msg-streaming-ph");
		if (streamingPh) streamingPh.remove();
	}
	// 追加新消息对象到 state.messages
	Array.prototype.push.apply(state.messages, newMsgs);
	// 同步聊天占位符（如果有新消息但最后一条非 assistant → 显示流式占位）
	syncStreamingPlaceholder();
}
// syncStreamingPlaceholder：同步聊天占位符的可见性
function syncStreamingPlaceholder() {
	var msgsEl = document.getElementById("chat-msgs");
	if (!msgsEl) return;
	var existing = msgsEl.querySelector(".msg-streaming-ph");
	var lastMsg = state.messages.length
		? state.messages[state.messages.length - 1]
		: null;
	var showPh =
		state.chatLoading &&
		(!lastMsg || lastMsg.role !== "assistant" || lastMsg._final);
	if (showPh && !existing) {
		var aiAvatar = '<img src="/mascot.webp" alt="小宅">';
		var pillHtml = "";
		(state.pendingTools || []).forEach((nm) => {
			pillHtml +=
				'<span class="thinking-tool"><svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>' +
				escHtml(nm) +
				"</span>";
		});
		msgsEl.insertAdjacentHTML(
			"beforeend",
			'<div class="msg msg-assistant msg-streaming-ph"><div class="msg-avatar">' +
				aiAvatar +
				'</div><div class="msg-content"><div class="msg-bubble"><span class="live-spinner"></span>' +
				(pillHtml
					? '<span class="thinking-tools">' + pillHtml + "</span>"
					: "") +
				"</div></div></div>",
		);
	} else if (!showPh && existing) {
		existing.remove();
	}
}

async function loadTerminals() {
	try {
		var data = await api("/terminals");
		if (data && data.terminals) state.terminals = data.terminals;
	} catch (e) {}
}

async function loadCmdHistory() {
	try {
		var data = await api("/cmd/history");
		if (data && data.history) state.cmdHistory = data.history;
	} catch (e) {}
}

function appendTermBuf(el, text) {
	if (!text) return;
	el.textContent += text;
	if (el.textContent.length > 262144) {
		el.textContent = el.textContent.slice(el.textContent.length - 262144);
	}
	el.scrollTop = el.scrollHeight;
}

function renderTerminals() {
	var r = document.getElementById("term-list");
	var cnt = document.getElementById("term-count-badge");
	if (!r) return;
	var list = state.terminals || [];
	if (cnt) cnt.textContent = list.length;
	if (list.length === 0) {
		r.innerHTML =
			'<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:11px">' +
			__("暂无终端会话", "No terminal sessions") +
			"</p>";
		return;
	}
	var html = "";
	list.forEach((t, i) => {
		var detailId = "term-detail-" + i;
		var scr = (state.termScreens && state.termScreens[t.id]) || null;
		var running = scr ? scr.running : !!t.running;
		var fullOut = scr ? scr.output : t.output || "";
		if (!fullOut) {
			fullOut =
				'<span style="color:#5c6672">' +
				__("[终端暂无输出]", "[No terminal output]") +
				"</span>";
		} else {
			fullOut = escHtml(fullOut);
		}
		html +=
			'<div style="border:1px solid var(--border-color);border-radius:6px;margin-bottom:4px;font-size:11px">';
		html +=
			'<div style="display:flex;align-items:center;gap:6px;padding:6px 8px;cursor:pointer;background:var(--bg-hover)" onclick="var d=document.getElementById(\'' +
			detailId +
			"');d.style.display=d.style.display==='none'?'block':'none'\">";
		html +=
			'<span style="font-family:monospace;font-size:10px;flex:1">' +
			escHtml(t.id || "-") +
			"</span>";
		html +=
			'<span style="flex:1;color:var(--text-muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' +
			escHtml(t.command || "") +
			"</span>";
		html +=
			'<span class="badge ' +
			(running ? "badge-green" : "badge-red") +
			'">' +
			(running ? __("运行中", "Running") : __("已关闭", "Closed")) +
			"</span>";
		html +=
			'<span style="color:var(--text-muted);font-size:10px">' +
			escHtml(t.created_at || "") +
			"</span>";
		html += "</div>";
		html +=
			'<div id="' +
			detailId +
			'" style="display:none;padding:8px;border-top:1px solid var(--border-color);background:var(--bg-input)">';
		html += '<div class="term-screen">';
		html +=
			'<div class="term-head"><span class="term-dot' +
			(running ? "" : " stopped") +
			'" id="term-dot-' +
			escHtml(t.id) +
			'"></span><span style="font-weight:600">' +
			escHtml(t.id) +
			"</span><span>" +
			escHtml(t.command || "") +
			'</span><span style="flex:1"></span><span>' +
			escHtml(t.uptime || "") +
			"</span></div>";
		html +=
			'<pre class="term-buf" id="term-buf-' +
			escHtml(t.id) +
			'">' +
			fullOut +
			"</pre>";
		html += "</div></div></div>";
	});
	r.innerHTML = html;
}

function renderCmdHistory() {
	// 面板标题是「命令历史」，数据源必须是 cmd_run 的执行记录
	// （/api/v1/cmd/history → 内核 TerminalRegistry.CmdHistory）。
	//
	// ★ 旧实现渲染的是**终端列表**（state.terminals 里 running 的那些），
	// 而 loadCmdHistory() 拉回来的 state.cmdHistory **从未被读过** ——
	// 于是面板名叫「命令历史」，实际只显示运行中的终端：
	//   · 已完成的命令（绝大多数）永远不出现，用户以为命令没执行；
	//   · 终端的 uptime 被当成命令时长，语义也对不上。
	// 现在分两段：先列 cmd_run 历史（带退出码/输出），再列运行中的终端。
	var r = document.getElementById("cmd-list");
	var cnt = document.getElementById("cmd-count-badge");
	if (!r) return;

	var history = state.cmdHistory || [];
	var running = (state.terminals || []).filter((t) => t.running);
	if (cnt) cnt.textContent = String(history.length + running.length);

	if (history.length === 0 && running.length === 0) {
		r.innerHTML =
			'<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:11px">' +
			__("暂无命令记录", "No command history") +
			"</p>";
		return;
	}

	var html = "";

	if (history.length > 0) {
		html +=
			'<table style="font-size:10px"><tr><th>' +
			__("命令", "Command") +
			"</th><th>" +
			__("状态", "Status") +
			"</th><th>" +
			__("时间", "Time") +
			"</th></tr>";
		// 新的在上：内核按时间正序追加，用户最关心最近一条。
		history
			.slice()
			.reverse()
			.forEach((c) => {
				var ok = (c.status || "").toLowerCase() === "ok" || c.exit_code === 0;
				var badge = ok ? "badge-green" : "badge-red";
				var label = ok
					? __("成功", "OK")
					: __("失败", "Failed") + (c.exit_code ? " (" + c.exit_code + ")" : "");
				html +=
					"<tr>" +
					'<td style="font-family:monospace;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' +
					escHtml(c.command || "-") +
					"</td>" +
					'<td><span class="badge ' +
					badge +
					'">' +
					escHtml(label) +
					"</span></td>" +
					'<td style="color:var(--text-muted);white-space:nowrap">' +
					escHtml(c.time || "-") +
					"</td>" +
					"</tr>";
				// 输出按需展开：默认收起，避免长输出把面板撑爆。
				var out = (c.stdout || "") + (c.stderr ? "\n" + c.stderr : "");
				out = out.trim();
				if (out) {
					html +=
						'<tr><td colspan="3" style="padding:0 0 4px 0">' +
						'<pre style="margin:0;padding:4px 8px;max-height:120px;overflow:auto;background:var(--bg-input);border-radius:4px;font-size:10px;color:var(--text-secondary);white-space:pre-wrap">' +
						escHtml(out.substring(0, 2000)) +
						(out.length > 2000 ? "\n… (truncated)" : "") +
						"</pre></td></tr>";
				}
			});
		html += "</table>";
	}

	if (running.length > 0) {
		html +=
			'<div style="margin-top:8px;font-size:10px;color:var(--text-muted)">' +
			__("运行中的终端", "Running terminals") +
			"</div>";
		html +=
			'<table style="font-size:10px"><tr><th>' +
			__("命令", "Command") +
			"</th><th>" +
			__("运行时长", "Uptime") +
			"</th></tr>";
		running.forEach((t) => {
			html +=
				"<tr>" +
				'<td style="font-family:monospace;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' +
				escHtml(t.command || t.id || "") +
				"</td>" +
				'<td style="color:var(--text-muted);white-space:nowrap">' +
				escHtml(t.uptime || "-") +
				"</td>" +
				"</tr>";
		});
		html += "</table>";
	}

	r.innerHTML = html;
}

function connectSSE() {
	if (state.eventSource) {
		console.log("[SSE] closing old connection");
		state.eventSource.close();
	}
	var es;
	try {
		es = new EventSource("/api/v1/chat/events", {
			withCredentials: true,
		});
		state.eventSource = es;
	} catch (ex) {
		console.error("[SSE] create failed", ex);
		return;
	}
	if (!es) {
		console.error("[SSE] es is null");
		return;
	}
	console.log("[SSE] connected");
	es.addEventListener("agent_output", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			// 光点：agent → 输出通道。总览页拓扑靠它做"消息出去了"的动画；
			// 其它页没有该 SVG，rtSparkOutput 查不到元素就是 no-op。
			if (p.channel) rtSparkOutput(ev.source || "", p.channel);
			// 星图跟随：agent 输出是一次完整活动的收尾，给一记强脉冲。
			starmapPulse("output", p.channel || null);
			console.log(
				"[SSE] agent_output received",
				p.content ? p.content.substring(0, 50) : "(empty)",
			);
			if (!p.content) return;
			state.chatStage = __("AI 回复中...", "AI replying...");
			if (p.kind === "channel_output") {
				// 附件输出（output_type=image/file）：渲染为图片预览/下载卡片
				var att = null;
				if (
					p.output_type === "image" ||
					p.output_type === "audio" ||
					p.output_type === "file"
				) {
					att = {
						type: p.output_type,
						url: p.url || p.content,
						size: p.size || 0,
						name: (p.url || "").split("/").pop() || "附件",
					};
				}
				var cm = {
					role: "assistant",
					content: att ? "" : p.content,
					source: p.channel || "",
					_final: true,
					_grow: true,
					attachment: att,
				};
				if (
					state.chatFinalIdx >= 0 &&
					state.chatFinalIdx < state.messages.length
				) {
					state.messages.splice(state.chatFinalIdx, 0, cm);
					state.chatFinalIdx++;
				} else {
					state.messages.push(cm);
				}
				rerenderChat();
				return;
			}
			var lastM2 = state.messages.length
				? state.messages[state.messages.length - 1]
				: null;
			if (lastM2 && lastM2.role === "assistant" && !lastM2._final) {
				// 聚合最终响应：覆盖 delta 累积的中间内容（以聚合为准，
				// 含 stage 插件改写后的最终文本），并置 final 结束本轮流式。
				lastM2._grow = true;
				lastM2.content = p.content;
				lastM2._final = true;
				rerenderChat();
				endChatTurn();
				return;
			}
			if (
				lastM2 &&
				lastM2.role === "assistant" &&
				lastM2._final &&
				!lastM2.source
			) {
				// 去重：同一轮的重复帧（如 SSE 重连回放）内容相同则忽略；
				// 内容不同视为新一轮输出（上一轮已 final 且无 source），开新消息。
				// 旧逻辑无条件 return 会丢弃多轮连发时新一轮的最终回复。
				if (lastM2.content === p.content) {
					endChatTurn();
					return;
				}
			}
			state.messages.push({
				role: "assistant",
				content: p.content,
				_streaming: true,
				_grow: true,
				_final: true,
			});
			rerenderChat();
			endChatTurn();
		} catch (ex) {
			console.error("[SSE] agent_output error", ex);
		}
	});
	// 用量账目：内核在每次 LLM 调用记账后发 agent_llm_chain，
	// payload 同时带 usage（本次）与 usage_session（会话累计）。
	//
	// 为何要显式订阅：后端**一直在推**这个事件（见 handler_chat.go 的 subTypes），
	// 而前端从未监听它 —— 另一个「写了、发了、不报错，只是没人收到」。
	// 后果是用户看不到缓存命中率与 token 用量。
	es.addEventListener("agent_llm_chain", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			renderChatUsage(p.usage_session, p.usage, __("本会话", "this session"));
		} catch (ex) {
			console.error("[SSE] agent_llm_chain error", ex);
		}
	});

	// token 级流式增量：逐块追加到当前回复内容（流式生成中）；
	// reset 帧表示轮次作废（用户中断）：定格已显示的部分内容，置 final。
	es.addEventListener("content_delta", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			if (p.channel === "_consolidation_") return;
			if (p.reset) {
				var lm = state.messages.length
					? state.messages[state.messages.length - 1]
					: null;
				if (lm && lm.role === "assistant" && !lm._final) {
					lm._final = true;
					rerenderChat();
				}
				// 轮次作废（用户中断）：定格已显示内容；核心会以 [中断消息]
				// 重启轮次，保持回合打开让确认回复继续流式渲染，
				// 由其 agent_output final / watchdog 收尾。
				armTurnWatchdog();
				return;
			}
			if (!p.content) return;
			state.chatStage = __("AI 回复中...", "AI replying...");
			var last =
				state.messages.length > 0
					? state.messages[state.messages.length - 1]
					: null;
			if (!last || last.role !== "assistant" || last._final) {
				state.messages.push({
					role: "assistant",
					content: "",
					tool_calls: [],
					_streaming: true,
					_grow: true,
				});
				last = state.messages[state.messages.length - 1];
			}
			last.content += p.content;
			rerenderChat();
		} catch (ex) {}
	});
	es.addEventListener("terminal_output", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			if (!p.terminal_id) return;
			var id = p.terminal_id;
			if (!state.termScreens) state.termScreens = {};
			var scr =
				state.termScreens[id] ||
				(state.termScreens[id] = { output: "", running: true });
			if (p.output) scr.output += p.output;
			if (typeof p.running === "boolean") scr.running = p.running;
			var bufel = document.getElementById("term-buf-" + id);
			if (bufel) {
				appendTermBuf(bufel, p.output || "");
				var dot = document.getElementById("term-dot-" + id);
				if (dot) dot.className = "term-dot" + (scr.running ? "" : " stopped");
			}
		} catch (ex) {
			console.error("[SSE] terminal_output error", ex);
		}
	});
	es.addEventListener("reasoning", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			if (p.channel === "_consolidation_") return;
			if (p.content) {
				state.chatStage = __("AI 思考中...", "AI thinking...");
				var last =
					state.messages.length > 0
						? state.messages[state.messages.length - 1]
						: null;
				if (!last || last.role !== "assistant" || last._final) {
					state.messages.push({
						role: "assistant",
						content: "",
						reasoning_content: "",
						tool_calls: [],
						_streaming: true,
					});
					last = state.messages[state.messages.length - 1];
				}
				// 聚合 reasoning 帧携带全文：直接覆盖（若已有 delta 累积则等价）
				last.reasoning_content = p.content;
				rerenderChat();
			}
		} catch (ex) {}
	});
	// token 级流式增量：逐块追加到当前思考内容；reset 帧表示轮次作废
	es.addEventListener("reasoning_delta", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			if (p.channel === "_consolidation_") return;
			if (p.reset) return; // 轮次作废（用户中断）：清空累积中的思考
			if (!p.content) return;
			state.chatStage = __("AI 思考中...", "AI thinking...");
			var last =
				state.messages.length > 0
					? state.messages[state.messages.length - 1]
					: null;
			if (!last || last.role !== "assistant" || last._final) {
				state.messages.push({
					role: "assistant",
					content: "",
					reasoning_content: "",
					tool_calls: [],
					_streaming: true,
				});
				last = state.messages[state.messages.length - 1];
			}
			last.reasoning_content = (last.reasoning_content || "") + p.content;
			rerenderChat();
		} catch (ex) {}
	});
	es.addEventListener("tool_call", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			console.log("[SSE] tool_call", p);
			if (!p.tool) return;
			var last =
				state.messages.length > 0
					? state.messages[state.messages.length - 1]
					: null;
			if (!last || last.role !== "assistant" || last._final) {
				state.messages.push({
					role: "assistant",
					content: "",
					tool_calls: [],
					_streaming: true,
				});
				last = state.messages[state.messages.length - 1];
			}
			if (!last.tool_calls) last.tool_calls = [];
			last.tool_calls.push({
				tool: p.tool,
				name: p.tool,
				args: p.args || {},
				result: p.result || "",
				status: p.status || "ok",
				plugin: p.plugin || "",
			});
			var pidx = (state.pendingTools || []).indexOf(p.tool);
			if (pidx !== -1) state.pendingTools.splice(pidx, 1);
			state.chatStage = __("工具调用: ", "Tool: ") + (p.tool || "");
			// 星图跟随：工具调用是最可靠的活动信号。工具名（如
			// knowledge_list / qq_send）会拿去匹配相关实体节点并点亮。
			starmapPulse("tool", p.tool);
			rerenderChat();
		} catch (ex) {
			console.error("[SSE] tool_call error", ex);
		}
	});
	// 注：后端不发布 tool_result 类型事件（工具结果随 EventToolCall 一次发出），无此监听器。
	// memory_access：图记忆的**真实**读写（哪个块被读/写/删/合）。
	//
	// ★ 为何它比 tool_call 可靠：tool_call 只带工具名（memory_recall），
	//   而星图要的是**具体块 ID**。两者间没有映射——前端曾试着把工具名拆词
	//   去匹配实体名，而实体名是中文概念（小宅/对话/待命）、工具名是英文，
	//   实测每个工具名命中 0 个节点，于是每次都退回「亮 mention_count 前 8 个」，
	//   即无论 agent 在干什么，亮起的都是同一批无关节点。
	//
	// 这条事件由**图数据库自己在读写处上报**（internal/memory 的 AccessHook），
	// 携带真实块 ID ⇒ 反馈与动作一一对应。
	es.addEventListener("memory_access", (e) => {
		try {
			smOnMemoryAccess(JSON.parse(e.data));
		} catch (ex) {
			console.error("[SSE] memory_access error", ex);
		}
	});
	es.addEventListener("stage", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			var phase = p.phase || "";
			var tool = p.tool || "";
			if (p.channel === "_consolidation_") return;
			console.log("[SSE] stage event", phase, tool);
			// 驱动总览页的"阶段管道"滑块：滑到当前阶段；一轮跑完（或 2.5s 无新
			// 事件）自动回到空闲，避免留下一个永远停在 after_output 的假状态。
			// 阶段轨迹：本轮真实发生过什么，交给阶段管道画成 chip 序列。
			// 工具调用会反复出现（多轮 toolcall），output_* 单独配色。
			if (phase === "on_input") {
				state.stageTrail = [];
				rtTrailPush(0, "stage", __("输入", "input"), __("输入", "input"));
			} else if (phase === "pre_action") {
				rtTrailPush(
					1,
					"stage",
					__("组装上下文并思考", "assemble context and think"),
					__("思考", "think"),
				);
			} else if (phase === "before_toolcall" && tool) {
				var isOut = tool.indexOf("output_") === 0;
				rtTrailPush(2, isOut ? "output" : "tool", tool, rtShortTool(tool));
				state.toolFlash = true;
			} else if (phase === "before_output") {
				rtTrailPush(
					3,
					"stage",
					__("生成回复", "generate reply"),
					__("生成", "gen"),
				);
			} else if (phase === "after_output") {
				rtTrailPush(
					4,
					"stage",
					__("本轮完成", "turn complete"),
					__("完成", "done"),
				);
			}
			// 星图跟随：阶段推进也作为活动信号（工具名优先匹配节点）。
			starmapPulse("stage", tool || phase);
			state.pipelinePhase = phase;
			if (state.pipelineTimer) clearTimeout(state.pipelineTimer);
			state.pipelineTimer = setTimeout(() => {
				state.pipelinePhase = "";
				if (document.getElementById("rt-sec-pipe")) renderRuntime();
			}, 2500);
			if (document.getElementById("rt-sec-pipe")) renderRuntime();
			// 渲染后再放动画：morph 已经把 chip 文本更新成最新那条。
			if (state.toolFlash) {
				state.toolFlash = false;
				rtFlashLatestTool();
			}
			if (phase === "pre_action") {
				state.chatStage = __("AI 思考中...", "AI thinking...");
			} else if (phase === "before_toolcall") {
				state.chatStage = __("工具调用: ", "Tool: ") + (tool || "");
				if (tool && (state.pendingTools || []).indexOf(tool) === -1) {
					if (!state.pendingTools) state.pendingTools = [];
					state.pendingTools.push(tool);
					rerenderChat();
				}
			} else if (phase === "before_output") {
				state.chatStage = __("生成回复中...", "Generating response...");
			}
			var badge = document.getElementById("chat-stage");
			if (badge) {
				badge.textContent = state.chatStage || "";
				badge.style.display = "none";
			}
		} catch (ex) {
			console.error("[SSE] stage error", ex);
		}
	});
	// channel_input：内核在输入进来时另发的一条轻量事件（只带通道名与 agent
	// id，不带正文）。总览页拓扑靠它画"光点进入 agent"。
	es.addEventListener("channel_input", (e) => {
		try {
			var d = JSON.parse(e.data);
			var src = d.source || "";
			if (src) rtSparkInput(src);
		} catch (ex) {
			console.error("[SSE] channel_input error", ex);
		}
	});
	es.onopen = () => {
		console.log("[SSE] connection opened");
	};
	es.onerror = (e) => {
		console.error("[SSE] error", e);
		// 1) 立即 close 阻止浏览器原生自动重连与手动 setTimeout(connectSSE) 双连接竞态
		try {
			state.eventSource && state.eventSource.close();
			state.eventSource = null;
		} catch (ex) {}
		// 2) 连接错误期间可能丢失事件，增量补拉历史（无闪烁）
		syncChatFromHistory().catch(() => {});
		// 3) 2s 后手动重连（比原 5s 更快恢复）
		setTimeout(connectSSE, 2000);
	};
	// sync_required：Server 因 Last-Event-ID 不在 ring（delta ID / 已到 tip）无法重放，
	// 通知前端增量补拉历史——避免前端空等后续聚合事件导致「消息同步不及时」。
	es.addEventListener("sync_required", (e) => {
		console.log("[SSE] sync_required received, incremental sync");
		syncChatFromHistory().catch(() => {});
	});
	// system：内核级状态事件（目前唯一发布者是 plugin_crashed）。
	//
	// ★ 为何必须补上（2026-10-13 协议审计）：这个事件一直在发
	//   （internal/plugin/dynamic_proc.go:175），该处注释还写着「webui/诊断插件
	//   可订阅」，但 SSE 白名单里没有它、前端也没有订阅 —— 事件发出来就掉地上。
	//   后果：插件崩溃重启对用户完全不可见，只能去翻 journal 才知道
	//   （而“插件炸了”正是用户最该被告诉的事）。
	//
	// 处理分寸：用 toast 一次性告知，不进聊天流 —— 它不属于任何一轮对话，
	// 插进消息列表会打乱会话语义（用户会以为自己发过什么）。
	es.addEventListener("system", (e) => {
		try {
			var ev = JSON.parse(e.data);
			var p = ev.payload || {};
			if (p.event === "plugin_crashed") {
				var nm = p.plugin || "?";
				try {
					// toast(m, isError)：第二参是**布尔**，传字符串会被当 falsy。
					// 这里用 true：插件崩溃是需要用户知道的问题，不是普通提示。
					toast(
						__(
							"插件 " + nm + " 异常退出，内核正在重启它",
							"Plugin " + nm + " crashed; kernel is restarting it",
						),
						true,
					);
				} catch (_) {}
			}
		} catch (ex) {
			/* 诊断事件解析失败不应打断聊天流 */
		}
	});
	// 终端 / 运行中命令的定时刷新。
	//
	// ★ 原来这段是**无条件**的 5s 轮询（浏览器实测：停在总览页 40s 内
	// 打了 8 次 /terminals + 8 次 /cmd/history）。但这两个面板只存在于
	// **聊天页**（buildChatLayout 里的 chat-panel-terminal / -cmd），
	// 在总览/设置/内核页渲染它们既没人看也只改看不见的 DOM。
	//
	// 改为「仅聊天页可见时才轮询」，且错过的那一拍在切回聊天页时由
	// renderAll 补上（renderAll 的 terminals/cmdHistory 数据块带节流）。
	if (state._sidebarRefresh) clearInterval(state._sidebarRefresh);
	state._sidebarRefresh = setInterval(() => {
		var tab = document.querySelector("#tab-chat");
		if (!tab || !tab.classList.contains("active")) return;
		fetchTerminalsAndCmd().catch(() => {});
	}, 5000);
}

// fetchTerminalsAndCmd 拉终端与运行中命令并重渲染（只在聊天页需要）。
async function fetchTerminalsAndCmd() {
	try {
		var td = await api("/terminals");
		if (td && td.terminals) state.terminals = td.terminals;
	} catch (e) {}
	try {
		var ch = await api("/cmd/history");
		if (ch && ch.history) state.cmdHistory = ch.history;
	} catch (e) {}
	renderTerminals();
	renderCmdHistory();
}

// ===== Plugins =====
// 「服务入口」：被反代的插件服务，点一下直接打开。
//
// 为什么单独一张卡而不是塞进每个插件的详情：入口是**跨插件**的（同一个
// webui 端口、不同子域），用户的心智是「我要打开某个服务」，不是
// 「我要进某个插件的管理页」。
//
// 链接带 ?__token=<api_key>：子域与门户不同源，浏览器不会带上会话 cookie；
// 不带 token 会 401（这是刻意的受保护默认）。页面已登录，此处复用同一把 key。
function renderProxyServicesCard() {
	var svcs = state.proxyServices || [];
	var base = state.proxyBaseDomain || "localhost";
	var html =
		'<div class="card"><h2>' + __("服务入口", "Service Entry Points") + "</h2>";
	if (!svcs.length) {
		return (
			html +
			'<p style="color:var(--text-muted);font-size:13px">' +
			__(
				"暂无被反代的插件服务。插件在 plugin.json 的 proxies 字段里声明后会自动出现在这里。",
				"No proxied plugin services yet. Declare them in plugin.json's proxies field.",
			) +
			"</p></div>"
		);
	}
	var token = state.settings?.["plugin.webui.api_key"] || "";
	html +=
		'<div style="font-size:12px;color:var(--text-muted);margin-bottom:10px">' +
		__(
			"插件服务经 HologramAgent 同一端口反代。子域形态需要 DNS 能解析 ",
			"Proxied through the same port. The subdomain form needs DNS for ",
		) +
		"<b>" +
		escHtml("*." + base) +
		"</b>" +
		__(
			"；路径形态无 DNS 依赖，穿透场景下更可靠（点「打开」优先用它）。",
			"; the path form has no DNS dependency and is more reliable behind a tunnel (Open prefers it).",
		) +
		"</div>";
	html += '<div class="svc-list">';
	svcs.forEach((s) => {
		var url = s.url || "";
		if (url && token) url += "?__token=" + encodeURIComponent(token);
		var dot = s.ok ? "var(--ok, #22c55e)" : "var(--danger, #d1383d)";
		html +=
			'<div class="svc-row">' +
			'<span class="svc-dot" style="background:' +
			dot +
			'"></span>' +
			'<span class="svc-name">' +
			escHtml(s.plugin_name || s.plugin) +
			"</span>" +
			'<span class="svc-sub">' +
			escHtml(s.name) +
			"</span>" +
			(s.websocket ? '<span class="svc-tag">WS</span>' : "") +
			(s.auth === "none"
				? '<span class="svc-tag svc-tag-warn">' +
					__("未保护", "unprotected") +
					"</span>"
				: "") +
			'<span class="svc-path">' +
			escHtml(s.host + "." + base) +
			"</span>";
		// 「打开」优先用**路径形态**（url_portal）：
		//   - 它没有 DNS 依赖，单端口穿透 / 子域无证书时都能用；
		//   - 子域形态要求 DNS 能解析 <host>.<基域名>，而这些域名
		//     在外部常常不可达（实测：外层只放行一个 Host，三级子域
		//     因通配证书不匹配而握手失败）。
		// 保留子域链接作为次选（局域网内直连时它更直观）。
		var primary = s.url_portal || url;
		if (s.ok && primary) {
			html +=
				'<a class="btn btn-primary btn-sm" style="margin-left:auto" target="_blank" rel="noopener" href="' +
				escHtml(primary) +
				'">' +
				__("打开", "Open") +
				"</a>";
			if (url && s.url_portal && url !== s.url_portal) {
				html +=
					'<a class="btn btn-ghost btn-sm" target="_blank" rel="noopener" title="' +
					escHtml(url) +
					'" href="' +
					escHtml(url) +
					'">' +
					__("子域", "subdomain") +
					"</a>";
			}
		} else {
			html +=
				'<span class="svc-err" title="' +
				escHtml(s.error || "") +
				'">' +
				escHtml(s.error || __("不可用", "unavailable")) +
				"</span>";
		}
		html += "</div>";
	});
	html += "</div></div>";
	return html;
}

async function loadProxyServices() {
	try {
		var d = await api("/proxy/services");
		state.proxyServices = (d && d.services) || [];
		state.proxyBaseDomain = (d && d.base_domain) || "localhost";
	} catch (e) {
		state.proxyServices = [];
	}
}

function renderPlugins() {
	var k = state.kernel;
	var plugins = k?.plugins || [];
	var tools = k?.tools || [];
	var installed = state.installedPlugins || [];
	var html = renderProxyServicesCard();
	html +=
		'<div class="card"><h2>' +
		__("安装插件", "Install Plugin") +
		"</h2>" +
		'<div style="display:flex;gap:8px;margin-bottom:8px">' +
		'<input id="plugin-url" placeholder=".hmap ' +
		__("包下载 URL", "Package URL") +
		'" style="flex:1" onkeydown="if(event.key==\'Enter\')installPlugin()">' +
		'<button class="btn btn-primary" onclick="installPlugin()">' +
		__("安装", "Install") +
		"</button></div>" +
		'<div><input type="file" id="plugin-file" accept=".hmap" style="display:inline;width:auto" onchange="installPluginFile(this.files[0])">' +
		'<label for="plugin-file" class="btn btn-ghost" style="cursor:pointer">' +
		__("选择 .hmap 文件上传", "Upload .hmap file") +
		"</label></div></div>";
	var disabledNames = {};
	(state.disabledPlugins || []).forEach((d) => {
		disabledNames[d.name] = d;
	});
	var installedNames = (state.installedPlugins || []).map((p) => p.name);
	// 安装清单（plugin.json + 运行期）按名字索引：它带着 auto_start /
	// runtime_stopped / alive，是区分「按需待命」「运行态已停」「该起没起」
	// 的唯一数据源。内核 status 只给 loaded，光看它三者会被混成「未加载」。
	var installedByName = {};
	(state.installedPlugins || []).forEach((p) => {
		installedByName[p.name] = p;
	});
	var allPluginNames = {};
	plugins.forEach((p) => {
		allPluginNames[p.name] = true;
	});
	state.disabledPlugins.forEach((d) => {
		allPluginNames[d.name] = true;
	});
	installedNames.forEach((n) => {
		allPluginNames[n] = true;
	});
	html +=
		'<div class="card"><h2>' +
		__("所有插件", "All Plugins") +
		" (" +
		Object.keys(allPluginNames).length +
		")</h2>";
	var names = Object.keys(allPluginNames).sort();
	if (names.length === 0) {
		html +=
			'<div class="empty-state"><p>' +
			__("暂无插件", "No plugins") +
			"</p></div>";
	} else {
		html +=
			"<table><tr><th>" +
			__("名称", "Name") +
			"</th><th>" +
			__("状态", "Status") +
			"</th><th>" +
			__("操作", "Actions") +
			"</th></tr>";
		names.forEach((name) => {
			var isExternal = installedNames.indexOf(name) >= 0;
			var isDisabled = disabledNames[name];
			var meta = installedByName[name] || {};
			var loaded = plugins.some((p) => p.name === name);
			// auto_start 缺省 true（内置插件与老清单都按自启算）。
			var autoStart = meta.auto_start !== false;
			var runtimeStopped = meta.runtime_stopped === true;
			var crashed = loaded && meta.alive === false;
			// 状态判定必须覆盖五种真实形态，缺一种就会把正常状态显示成故障：
			//   已禁用 / 进程丢失 / 已运行 / 按需待命 / 该起没起
			var statusHtml;
			if (isDisabled && loaded) {
				statusHtml =
					'<span class="badge badge-yellow">' +
					__("运行中(禁用待生效)", "Running (disable pending)") +
					"</span>";
			} else if (isDisabled) {
				statusHtml =
					'<span class="badge badge-red">' +
					__("已禁用", "Disabled") +
					"</span>";
			} else if (crashed) {
				statusHtml =
					'<span class="badge badge-red">' +
					__("进程丢失", "Process lost") +
					"</span>";
			} else if (loaded) {
				statusHtml =
					'<span class="badge badge-green">' +
					(runtimeStopped
						? __("已运行(停止待生效)", "Running (stop pending)")
						: __("已运行", "Running")) +
					"</span>";
			} else if (!autoStart) {
				// 按需插件不运行是**正常待命**，不是故障。旧版一律显示
				// 「未加载」，用户以为插件坏了，实际它在等 plugin_start。
				statusHtml =
					'<span class="badge badge-blue">' +
					__("按需待命", "On-demand") +
					"</span>";
			} else if (runtimeStopped) {
				statusHtml =
					'<span class="badge badge-yellow">' +
					__("已停止(运行态)", "Stopped (runtime)") +
					"</span>";
			} else {
				statusHtml =
					'<span class="badge badge-gray">' +
					__("未运行", "Not running") +
					"</span>";
			}
			statusHtml +=
				' <span class="badge badge-' +
				(autoStart ? "green" : "gray") +
				'">' +
				(autoStart ? __("自启", "auto") : __("按需", "on-demand")) +
				"</span>";
			var actionsHtml = "";
			if (loaded) {
				actionsHtml +=
					'<button class="btn btn-sm btn-warning" onclick="stopPlugin(\'' +
					escHtml(name) +
					'\')" style="margin-right:4px">' +
					__("停止", "Stop") +
					"</button>";
				actionsHtml +=
					'<button class="btn btn-sm btn-ghost" onclick="disablePlugin(\'' +
					escHtml(name) +
					'\')" style="margin-right:4px">' +
					__("禁用", "Disable") +
					"</button>";
			} else if (isDisabled) {
				actionsHtml +=
					'<button class="btn btn-sm btn-success" onclick="enablePlugin(\'' +
					escHtml(name) +
					'\')" style="margin-right:4px">' +
					__("启用", "Enable") +
					"</button>";
			} else {
				// 未运行但未禁用：一律走**运行态启动**，不碰持久禁用清单。
				actionsHtml +=
					'<button class="btn btn-sm btn-success" onclick="startPlugin(\'' +
					escHtml(name) +
					'\')" style="margin-right:4px">' +
					__("启动", "Start") +
					"</button>";
			}
			if (isExternal) {
				actionsHtml +=
					'<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' +
					escHtml(name) +
					"')\">" +
					__("卸载", "Unload") +
					"</button>";
			}
			html +=
				"<tr><td>" +
				escHtml(name) +
				"</td>" +
				"<td>" +
				statusHtml +
				"</td>" +
				"<td>" +
				actionsHtml +
				"</td></tr>";
		});
		html += "</table>";
	}
	html += "</div>";
	if (installed.length > 0) {
		html +=
			'<div class="card"><h2>' +
			__("已安装外部插件", "Installed Plugins") +
			" (" +
			installed.length +
			")</h2>" +
			"<table><tr><th>" +
			__("名称", "Name") +
			"</th><th>" +
			__("版本", "Version") +
			"</th><th>" +
			__("描述", "Description") +
			"</th><th>" +
			__("操作", "Actions") +
			"</th></tr>";
		installed.forEach((p) => {
			html +=
				"<tr><td>" +
				escHtml(p.name) +
				"</td>" +
				"<td>" +
				escHtml(p.version || "-") +
				"</td>" +
				"<td>" +
				escHtml((p.description || "").substring(0, 50)) +
				"</td>" +
				'<td><button class="btn btn-sm btn-ghost" onclick="showPluginInfo(\'' +
				escHtml(p.name) +
				"')\">" +
				__("详情", "Details") +
				"</button> " +
				'<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' +
				escHtml(p.name) +
				"')\">" +
				__("卸载", "Unload") +
				"</button></td></tr>";
		});
		html += "</table></div>";
	}
	if (state.pluginInfo) {
		html +=
			'<div class="card"><h2>' +
			__("插件详情", "Plugin Details") +
			": " +
			escHtml(state.pluginInfo.name) +
			"</h2>" +
			"<pre>" +
			escHtml(JSON.stringify(state.pluginInfo, null, 2)) +
			"</pre>" +
			'<button class="btn btn-ghost" onclick="closePluginInfo()">' +
			__("关闭", "Close") +
			"</button></div>";
	}
	if (tools.length > 0) {
		html +=
			'<div class="card"><h2>' +
			__("已注册工具", "Registered Tools") +
			" (" +
			tools.length +
			")</h2>" +
			'<div style="display:flex;flex-wrap:wrap;gap:4px">';
		tools.forEach((t) => {
			html +=
				'<span class="tool-badge" title="' +
				escHtml(t.description || "") +
				'">' +
				escHtml(t.name) +
				"</span>";
		});
		html += "</div></div>";
	}
	html +=
		'<div class="card"><h2>' +
		__("系统操作", "System Operations") +
		"</h2>" +
		'<button class="btn btn-primary" onclick="reloadPlugins()">' +
		__("重载插件", "Reload Plugins") +
		"</button></div>";
	html +=
		'<div class="card health-card"><div class="health-head"><h2>' +
		__("健康检查", "Health Check") +
		'</h2><button class="btn btn-ghost btn-sm" onclick="runHealthcheck()">' +
		__("运行", "Run") +
		'</button></div><div id="health-panel">';
	if (state.healthResult) {
		html += renderHealthResult(state.healthResult);
	} else {
		html +=
			'<p style="color:var(--text-muted);font-size:13px">' +
			__("尚未运行,点击右上角「运行」开始", "Not run yet, click Run to start") +
			"</p>";
	}
	html += "</div></div>";
	document.getElementById("tab-plugins").innerHTML = html;
}

async function loadInstalledPlugins() {
	try {
		state.installedPlugins = await api("/plugins");
	} catch (e) {
		state.installedPlugins = [];
	}
}

async function installPlugin() {
	var inp = document.getElementById("plugin-url");
	var url = inp?.value.trim();
	if (!url) {
		toast(__("请输入插件包 URL", "Please enter plugin URL"), true);
		return;
	}
	try {
		var raw = await api("/plugins", {
			method: "POST",
			body: JSON.stringify({ url: url }),
			raw: true,
		});
		var ct = raw.headers.get("content-type") || "";
		var r = ct.includes("json") ? await raw.json() : await raw.text();
		if (!raw.ok || (r && r.error)) {
			toast(
				__("安装失败: ", "Install failed: ") +
					((r && (r.error || r.details)) || "HTTP " + raw.status),
				true,
			);
			return;
		}
		toast(
			__("安装结果: ", "Install result: ") + (r.status || JSON.stringify(r)),
		);
		if (r.action === "reload_required")
			toast(
				__(
					"已安装，请点击「重载插件」加载",
					'Installed, click "Reload Plugins" to load',
				),
				false,
			);
		await loadInstalledPlugins();
		renderPlugins();
	} catch (e) {
		toast(__("安装失败: ", "Install failed: ") + e.message, true);
	}
}

async function installPluginFile(file) {
	if (!file) return;
	try {
		var r = await fetch("/api/v1/plugins", {
			method: "POST",
			body: file,
			headers: { "Content-Type": "application/octet-stream" },
		});
		var data = await r.json();
		toast(
			__("上传安装: ", "Upload install: ") +
				(data.status || JSON.stringify(data)),
		);
		if (data.action === "reload_required")
			toast(
				__(
					"已安装，请点击「重载插件」加载",
					'Installed, click "Reload Plugins" to load',
				),
				false,
			);
		loadInstalledPlugins();
		renderPlugins();
	} catch (e) {
		toast(__("上传失败: ", "Upload failed: ") + e.message, true);
	}
}

async function showPluginInfo(name) {
	try {
		state.pluginInfo = await api("/plugins/" + encodeURIComponent(name));
		renderPlugins();
	} catch (e) {
		toast(__("获取详情失败: ", "Get details failed: ") + e.message, true);
	}
}

function closePluginInfo() {
	state.pluginInfo = null;
	renderPlugins();
}

var removingPlugins = {};

async function removePlugin(name) {
	if (removingPlugins[name]) return; // 防重复点击
	if (
		!confirm(
			__("确定卸载插件", "Are you sure to unload plugin") +
				"「" +
				name +
				"」？",
		)
	)
		return;
	removingPlugins[name] = true;
	try {
		var raw = await api("/plugins/" + encodeURIComponent(name), {
			method: "DELETE",
			raw: true,
		});
		var ct = raw.headers.get("content-type") || "";
		var body = ct.includes("json") ? await raw.json() : await raw.text();
		if (!raw.ok) {
			var em = (body && (body.error || body.details)) || "HTTP " + raw.status;
			toast(__("卸载失败: ", "Unload failed: ") + em, true);
			// 内置插件或路径错误时刷新一次列表保持状态一致
			loadInstalledPlugins();
			renderPlugins();
			return;
		}
		toast(__("已卸载: ", "Unloaded: ") + (body.name || body.status || name));
		await loadInstalledPlugins();
		// 同步内核插件/禁用列表，确保列表与工具立即消失
		try {
			state.kernel = await api("/kernel");
			var s = await api("/settings");
			state.disabledPlugins = s.disabled_plugins || [];
		} catch (e2) {}
		renderPlugins();
	} catch (e) {
		toast(__("卸载失败: ", "Unload failed: ") + e.message, true);
	} finally {
		delete removingPlugins[name];
	}
}

async function disablePlugin(name) {
	if (name === "webui") {
		var r = confirm(
			__(
				"禁用 WebUI 后将无法通过 URL:端口访问此管理面板，若要重新启用需要通过 CLI 命令 /plugin enable webui 恢复。\n\n确定要禁用吗？",
				"Disabling WebUI will make this management panel inaccessible via URL:port. To re-enable, use CLI command /plugin enable webui.\n\nAre you sure?",
			),
		);
		if (!r) return;
	}
	try {
		var r = await api("/plugins/" + encodeURIComponent(name) + "/disable", {
			method: "POST",
		});
		toast(__("已禁用: ", "Disabled: ") + name);
		state.kernel = await api("/kernel");
		var s = await api("/settings");
		state.disabledPlugins = s.disabled_plugins || [];
		renderPlugins();
	} catch (e) {
		toast(__("禁用失败: ", "Disable failed: ") + e.message, true);
	}
}

// startPlugin / stopPlugin 是**运行态**启停（对应 plugin_start / plugin_stop）：
// 只改现在跑不跑，不写持久禁用清单。与 enable/disable（持久态）分开，
// 避免「点了一下启动，重启后行为却变了」。
async function startPlugin(name) {
	try {
		await api("/plugins/" + encodeURIComponent(name) + "/start", {
			method: "POST",
		});
		toast(__("已启动: ", "Started: ") + name);
		state.kernel = await api("/kernel");
		await loadInstalledPlugins();
		renderPlugins();
	} catch (e) {
		toast(__("启动失败: ", "Start failed: ") + e.message, true);
	}
}

async function stopPlugin(name) {
	if (name === "webui") {
		var r = confirm(
			__(
				"停止 WebUI 插件后此管理面板会立即断开，只能通过 CLI 命令 /plugin start webui 恢复。\n\n确定要停止吗？",
				"Stopping the WebUI plugin disconnects this panel immediately. Recover with CLI /plugin start webui.\n\nStop it?",
			),
		);
		if (!r) return;
	}
	try {
		await api("/plugins/" + encodeURIComponent(name) + "/stop", {
			method: "POST",
		});
		toast(__("已停止: ", "Stopped: ") + name);
		state.kernel = await api("/kernel");
		await loadInstalledPlugins();
		renderPlugins();
	} catch (e) {
		toast(__("停止失败: ", "Stop failed: ") + e.message, true);
	}
}

async function enablePlugin(name) {
	try {
		var r = await api("/plugins/" + encodeURIComponent(name) + "/enable", {
			method: "POST",
		});
		toast(__("已启用: ", "Enabled: ") + name);
		state.kernel = await api("/kernel");
		var s = await api("/settings");
		state.disabledPlugins = s.disabled_plugins || [];
		renderPlugins();
	} catch (e) {
		toast(__("启用失败: ", "Enable failed: ") + e.message, true);
	}
}

async function reloadPlugins() {
	try {
		var r = await api("/plugins/reload", { method: "POST" });
		toast(__("插件已重载", "Plugins reloaded"));
		state.kernel = await api("/kernel");
		renderPlugins();
	} catch (e) {
		toast(__("重载失败: ", "Reload failed: ") + e.message, true);
	}
}

async function runHealthcheck() {
	var panel = document.getElementById("health-panel");
	if (!panel) return;
	panel.innerHTML =
		'<div class="loading" style="margin:12px auto"></div><p style="text-align:center;color:var(--text-muted)">' +
		__("运行中...", "Running...") +
		"</p>";
	try {
		var r = await api("/kernel");
		var tools = r?.tools || [];
		var healthTool = tools.find((t) => t.name === "healthcheck");
		if (!healthTool) {
			panel.innerHTML =
				'<p style="color:var(--text-secondary)">' +
				__("healthcheck 工具未注册", "healthcheck tool not registered") +
				"</p>";
			return;
		}
		panel.innerHTML =
			'<p style="color:var(--text-secondary)">' +
			__(
				"通过 Agent 对话触发 healthcheck...",
				"Triggering healthcheck via Agent...",
			) +
			"</p>";
		var chatR = await api("/chat", {
			method: "POST",
			body: JSON.stringify({
				message: __(
					"请运行 healthcheck 工具进行全面健康检查并报告结果",
					"Please run the healthcheck tool for a full system check and report the results",
				),
			}),
		});
		panel.innerHTML =
			"<pre>" + escHtml(JSON.stringify(chatR, null, 2)) + "</pre>";
	} catch (e) {
		panel.innerHTML =
			'<p style="color:#fca5a5">' +
			__("错误: ", "Error: ") +
			escHtml(e.message) +
			"</p>";
		toast(__("健康检查失败: ", "Health check failed: ") + e.message, true);
	}
}

function renderHealthResult(r) {
	if (!r || !r.checks)
		return (
			'<p style="color:var(--text-secondary)">' +
			__("暂无健康检查数据", "No health check data") +
			"</p>"
		);
	var checks = r.checks || [];
	var passed = checks.filter((c) => c.pass).length;
	var failed = checks.filter((c) => !c.pass).length;
	var html =
		'<div style="margin-bottom:12px;display:flex;gap:16px;align-items:center">' +
		'<span class="badge badge-green">' +
		__("通过: ", "Pass: ") +
		passed +
		"</span>" +
		'<span class="badge ' +
		(failed > 0 ? "badge-red" : "badge-green") +
		'">' +
		__("失败: ", "Fail: ") +
		failed +
		"</span>" +
		'<span class="badge badge-blue">' +
		__("总计: ", "Total: ") +
		checks.length +
		"</span></div>";
	checks.forEach((c) => {
		var passClass = c.pass ? "check-pass" : "check-fail";
		if (c.status === "skip") passClass = "check-skip";
		html +=
			'<div class="health-item">' +
			'<span class="check-name">' +
			escHtml(c.name) +
			"</span>" +
			'<span class="check-status ' +
			passClass +
			'">' +
			(c.status || "unknown") +
			"</span>" +
			'<span style="color:var(--text-muted);font-size:11px">' +
			escHtml(c.detail || "") +
			"</span></div>";
	});
	return html;
}

// ===== Kernel =====
function renderKernel() {
	var k = state.kernel;
	if (!k) {
		document.getElementById("tab-kernel").innerHTML =
			'<div class="card"><p style="color:var(--text-muted)">' +
			__("内核未响应", "Kernel not responding") +
			"</p></div>";
		return;
	}
	var b = k.build || {};
	var html =
		'<div class="card"><h2>' +
		__("构建", "Build") +
		"</h2>" +
		'<div class="kv-row"><span class="key">' +
		__("内核", "Kernel") +
		'</span><span class="val">' +
		escHtml(b.kernel_name || "HologramAgent") +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("内核版本", "Kernel version") +
		'</span><span class="val">' +
		escHtml(b.version ? "v" + b.version : "-") +
		"</span></div>" +
		'<div class="kv-row"><span class="key">Commit</span><span class="val">' +
		escHtml(b.commit || "-") +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("构建时间", "Build time") +
		'</span><span class="val">' +
		escHtml(b.build_time || "-") +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("SDK 兼容", "SDK compat") +
		'</span><span class="val">' +
		escHtml(b.sdk_compatible || "-") +
		"</span></div>" +
		(b.source_url
			? '<div class="kv-row"><span class="key">' +
				__("源码", "Source") +
				'</span><span class="val"><a href="' +
				escHtml(b.source_url) +
				'" target="_blank" rel="noopener noreferrer">' +
				escHtml(b.source_url) +
				"</a></span></div>"
			: "") +
		"</div>";
	html +=
		'<div class="card"><h2>' +
		__("运行时", "Runtime") +
		"</h2>" +
		'<div class="kv-row"><span class="key">Goroutines</span><span class="val">' +
		(k?.runtime?.goroutines || "-") +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("内存", "Memory") +
		'</span><span class="val">' +
		(k?.runtime?.memory_mb ? k.runtime.memory_mb + " MB" : "-") +
		"</span></div>" +
		'<div class="kv-row"><span class="key">Go ' +
		__("版本", "Version") +
		'</span><span class="val">' +
		(k?.runtime?.go_version || "-") +
		"</span></div></div>";
	html +=
		'<div class="card"><h2>LLM</h2>' +
		'<div class="kv-row"><span class="key">Provider</span><span class="val">' +
		(k.llm?.provider || __("未配置", "Not configured")) +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("可用源", "Sources") +
		'</span><span class="val">' +
		(k.llm?.sources || 0) +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("状态", "Status") +
		'</span><span class="val"><span class="status-dot ' +
		(k.llm?.available ? "dot-green" : "dot-red") +
		'"></span>' +
		(k.llm?.available ? __("运行中", "Running") : __("不可用", "Unavailable")) +
		"</span></div></div>";
	html +=
		'<div class="card"><h2>' +
		__("记忆", "Memory") +
		"</h2>" +
		'<div class="kv-row"><span class="key">' +
		__("图记忆", "Graph Memory") +
		'</span><span class="val">' +
		(k.memory?.available
			? k.memory.entity_count +
				__(" 实体, ", " entities, ") +
				k.memory.relation_count +
				__(" 关系", " relations")
			: __("未初始化", "Uninitialized")) +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("文档记忆", "Document Memory") +
		'</span><span class="val">' +
		(k.documents?.available
			? k.documents.doc_count + __(" 文档", " docs")
			: __("未初始化", "Uninitialized")) +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("文本记忆", "Text Memory") +
		'</span><span class="val">' +
		(k.text_memory?.available
			? k.text_memory.file_count + __(" 文件", " files")
			: __("未初始化", "Uninitialized")) +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("知识库", "Knowledge") +
		'</span><span class="val">' +
		(k.knowledge?.available
			? k.knowledge.item_count + __(" 项", " items")
			: __("未初始化", "Uninitialized")) +
		"</span></div></div>";
	html +=
		'<div class="card"><h2>' +
		__("插件", "Plugins") +
		" (" +
		(k.plugins?.length || 0) +
		")</h2>";
	if (k.plugins?.length) {
		html += '<div style="display:flex;flex-wrap:wrap;gap:4px">';
		k.plugins.forEach((p) => {
			html += '<span class="badge badge-blue">' + escHtml(p.name) + "</span>";
		});
		html += "</div>";
	} else {
		html += '<p style="color:var(--text-muted)">' + __("无", "None") + "</p>";
	}
	html += "</div>";
	document.getElementById("tab-kernel").innerHTML = html;
}

// ===== Star Map =====
var starmapScene = null,
	starmapCam = null,
	starmapRen = null,
	starmapCtrl = null;
var starmapNodes = [],
	starmapEdges = [];
var starmapNodeMeshes = [],
	starmapEdgeLines = [],
	starmapStarField = null;
var starmapHovered = null,
	starmapSelected = null,
	starmapAutoView = true;
var starmapRaf = null;
// starmapAnimateRunning：渲染循环的开关。
//
// ★ 为何要显式开关而不是只靠 requestAnimationFrame：
//   RAF 一旦排上就会一直回调（除非 cancel）。本循环在**不可见时自行 return**，
//   若不用开关，那个 return 只是「不干活但仍在被 60fps 调用」，仍白耗一层
//   调度；而开关让它能真正停下，由 starmapWake 重新点火。
var starmapAnimateRunning = false;
// starmapLastRenderKey：空闲帧跳过的状态（2026-10-13）。
//
// 为何需要：3753 节点 = 3760 draw call/帧，静止画面重绘一遍是纯浪费
// （实测该环境下每帧把主线程占满）。判据见 starmapAnimate。
var starmapLastRenderKey = null;
// starmapLastRenderAt / starmapLastFrameTs：空闲心跳与星空时间驱动（2026-10-13）。
//
// ★ 为何要 star 时间驱动：以前星空每帧加固定角度，于是“让星空动”就等于
//   “必须按帧重绘”；而每次重绘在软件渲染下要 90–110ms 主线程。
//   改成按真实 dt 推进后，星空角度与重绘频率解耦，空闲时可以大幅降频。
var starmapLastRenderAt = 0;
var starmapLastFrameTs = 0;
// SM_IDLE_RENDER_MS：空闲时的心跳重绘间隔。
//
// 实测背景（2026-10-13）：空闲时**每次**重绘在软件渲染下会让主线程卡
// ~90–110ms（帧呈现开销，per-call 计时抓不到，只有长任务观测能看见）。
// 因此重绘间隔直接决定「卡不卡」：
//   0.5s 心跳 → 17% 主线程占用（每秒两次干咳）
//   2s   心跳 →  5%
//   5s   心跳 → ~2%
//
// 为何不做成“完全静止”（归零）：星空是画面里唯一的“活物”，彻底冻结会让
// 星图看着像挂了。5s 是“看得出在转”与“不卡”的折中（星空按真实 dt 推进，
// 所以降频不会让它跳变）。
//
// ★ 任何交互（拖动/悬停/点击）或活动脉冲都会改变 camKey ⇒ 立即重绘，
//   不受此间隔限制 —— 手感不会因此变粘。
var SM_IDLE_RENDER_MS = 5000;
// starmapDirty：图结构发生变化（新建/删除/合入块），需要重拉全量。
//
// ★ 必须显式声明。此前两处 `starmapDirty = true` 赋的是**隐式全局**，
//   而没有任何地方读它 ⇒ 新块永远不会出现在图上（星图看着“不长”）。
//   消费点在 starmapStartActivity 的 pulse 轮询里。
var starmapDirty = false;
// 星图布局 Worker（力导向布局跑在主线程之外，见 starmapRequestLayout）。
//
// ★ 2026-10-13 实测动机：布局原先在主线程同步跑，生产库 3754 块时
//   单次阻塞 10.9 秒（首帧 11.8 秒）。Worker 让主线程在计算期间完全不阻塞。
//   workerBroken 是「脚本加载失败」的标记 —— 此后走串行回退而不是反复重试。
var starmapLayoutWorker = null,
	starmapLayoutWorkerBroken = false,
	starmapLayoutSeq = 0,
	starmapLayoutPending = {};
// 共享几何：1151 节点各自 new SphereGeometry 会产生 1151 个
// BufferGeometry（另加同样数量的光晕球）。几何形状对所有节点相同，
// 差别只在外层 mesh.scale，故共享一份即可。半径固定 0.5，
// 真实半径由 scale 给出（见 buildChatStarmapGraph）。
var starmapGeo = null,
	starmapGlowGeo = null;
// starmapGlowMesh：全部节点的光晕合并成的一个 InstancedMesh（2026-10-13）。
//
// 为何是单实例：原来每节点一个光晕 mesh（3753 个），使 draw call 达 7512/帧。
// 光晕是纯静态装饰（不参与任何动画），故可合并。详注见 starmapBuildGlowInstances。
var starmapGlowMesh = null;
// starmapGlowItems：光晕实例的构建参数（id/scale/color）。
// 存下来是为了在异步布局回传后重建实例矩阵（位置变了）。
var starmapGlowItems = [];
// 标签角标：hover / 选中时显示的 HTML 元素（零显存，文字清晰）。
var starmapLabelEl = null;

// ===== 常驻节点名覆盖层（2026-10-08）=====
//
// ★ 为何不是“每节点一张 CanvasTexture”：那是本页原来的做法，1151 张
//   256x64 贴图 ≈ 72MB 显存，且贴在半径 0.5~2.5 的球上本来就糊。
//   改用**一个** canvas 覆盖层，每帧把节点名用 2D 文字画上去（同屏可见者
//   才画、带视锥裁剪与去重），显存零开销、文字反而更清晰。
//
// ★ 为何要“全部标名”：不标名时 3178 个球彼此无区别，星图看不出“这是什么”。
//   但全标会重叠成一团 ⇒ 用**字体与透明度分级**：命中/高亮的块名实色、
//   其余名字压低透明当底纹，鼠标悬浮时再上提亮当前那个。
var starmapLabelCanvas = null; // 覆盖层 canvas（叠在 WebGL canvas 上）
var starmapLabelCtx = null;
var starmapLabelCache = null; // { canvas, ctx, w, h } 惰性缓存，容器搬家时重建
// smTypeColors 的键必须与**服务端实际产出的 type 字符串小写后**一致。
// 服务端默认类型是 "Concept"（首字母大写，见 internal/memory/graph.go），
// 原键全为小写 ⇒ 永远匹配不上 ⇒ 1150 个节点全渲染成同一个灰色 0xcccccc，
// 分类配色实际上从未生效过。现已加小写归一化，映射是真的生效了。
//
// ★ 但色相换成**中性灰蓝**（用户裁定）：亮青绿（0x44ff88）配 1151 个
// 自发光球确实扎眼，而且它掩盖了一个更刺眼的事实 ——
//
//	实测图谱里 Concept 占 1148/1149 = 99%（nlp/extractor.go 完全不判类型，
//	graph.go:451 在 type 为空时写死 "Concept"）。也就是说这套按类型
//	配色的设计，对真实数据而言**只有一种颜色会被用到**。
//
//	所以真正的做法不是挑一个好看的色，而是：
//	  1. 默认色改为低饱和灰蓝（全图统一，不假装在分类）
//	  2. 类型色只在**真的存在多种类型**时才按类型区分（见 smColorFor）
//	  3. 图例按实际节点集合动态生成，不列出永不出现的类型
// ★ 2026-10-08 改为**灰色调**（用户要求）：
//
// 之前的分类型配色有它的道理（能区分类型），但真实图谱里 99% 是同一类
// （实测 Concept 1293 / 其他 1），那套配色对生产数据只用得上一种颜色；
// 而切到块体系后类型更杂（sentence/triple/legacy-entity/image…）又反而花了。
//
// 所以改为：**全图灰底，用亮度表达重要度**（度数越高越亮）。
// 好处是星图变成一张“热度图”——亮的地方就是记忆稠密的地方，
// 不需要读图例就能看出结构；而活动/悬浮/选中会在灰底上单独提亮（见
// starmapTickMorph 与 onStarmapMove），对比反而更强。
var SM_COLOR_DIM = 0xc8ced8; // 兼容旧引用：单一类型时的全图色
var smTypeColors = {
	// 保留类型色定义（图例仍会按实际出现的类型列出），但默认不再按类型上色。
	person: 0x8fa4bd,
	task: 0xa89b90,
	ai: 0x9a94ab,
	concept: 0xb9c0cb,
	object: 0xb5a5a5,
	location: 0xb8ad9a,
	source: 0xa3abb8,
	document: 0xa8aeb6,
	event: 0xb3a8b0,
	entity: 0xa4b8b5,
	scene: 0xacb8a4,
	// 块体系的实际类型（见 smAdaptGraph）
	sentence: 0xb0b6bf,
	triple: 0xb6bcc4,
	block: 0xbcc2ca,
	"legacy-entity": 0xa9afb8,
	image: 0xa6b0bd,
	audio: 0xa6b8b8,
};

// smGreyFor 返回一个灰阶色：亮度随重要度（度数归一化值）变化。
//
// t ∈ [0,1]：0 = 最暗（孤立节点），1 = 最亮（连接最多的节点）。
// 亮度区间刻意不拉到两端：纯黑节点在深色底上看不见，
// 纯白又会与“活动提亮”混淆——活动态要在灰底上看出差别。
function smGreyFor(t) {
	if (!(t >= 0)) t = 0;
	if (t > 1) t = 1;
	// 0x6e → 0xdc：深灰到亮灰。
	var v = Math.round(0x6e + (0xdc - 0x6e) * t);
	// 略偏冷（蓝多一档）以配合深色星空底，纯中性灰在这底上偏黄。
	return (v << 16) | (Math.min(255, v + 4) << 8) | Math.min(255, v + 12);
}

// smColorTypeSet 统计本次图谱里实际出现的类型（小写）。
// 由 buildChatStarmapGraph 在建图前填好。
var smPresentTypes = {};

// smColorFor 取节点颜色。
//
// ★ 现在固定返回灰阶（不再按类型上色），亮度由调用方按度数给出。
// 保留函数签名是为了不改动所有调用点（图例/建图都用它）。
//
// 为何不再按类型：真实图谱里 99% 是同一类，分类型配色在生产数据上
// 实质只用得上一种颜色；而块体系下类型又碎成 sentence/triple/… 反而更花。
// 改为灰阶 + 度数亮度后，星图变成热度图：亮处即记忆稠密处。
function smColorFor(type, t) {
	return smGreyFor(typeof t === "number" ? t : 0.5);
}
var smEdgeColors = {
	喜欢: 0xff6b6b,
	学习: 0x4ecdc4,
	属于: 0x45b7d1,
	相关: 0x96ceb4,
	使用: 0xfeca57,
	创建: 0xff9ff3,
};

// ===== 星图活动：让图跟上 agent 的动作 =====
//
// 改之前：starmapAnimate() 只转星空，节点完全静止。现在三路信号：
//
//  1. starmapPulse(kind)      —— 实时。SSE 的 tool_call / stage /
//     agent_output 触发。命中的节点（按工具名匹配已知实体，否则随机
//     取一批）做一次扩散涟漪 + 发光冲高。
//  2. starmapPullActivity()   —— /runtime 调度器快照，3s。
//     ready_queue_depth > 0 ⇒ 图整体「绷紧」（轻微缩放脉冲）；
//     interrupts/preempts 上升 ⇒ 高优先级别的红色电弧感闪烁。
//  3. starmapPullPulse()      —— /memory/graph/pulse，10s。
//     最近变动的实体（新增概念 / mention_count 变化）做一次
//     「生长」：从 0 缩放到正常大小，并留下余晖。
//
// 全部在渲染循环里推进，不额外起定时器。
var SM_PULSE_MS = 1400; // 单个脉冲的生命期
var SM_RIPPLE_R = 26; // 涟漪最大半径

// starmapPulse 发出一次活动脉冲。
// kind: "tool" | "stage" | "output" | "grow"
// ===== 图变化动画（插入/删除/合入）=====
//
// 时序设计（这是三者不互相打架的关键）：
//
//	插入  grow     : 0 → 1.0 弹回，1800ms（带过冲，像“长出来”）
//	删除  dissolve : 1.0 → 0 收缩 + 粒子飞散，1200ms
//	合入  merge    : 源块消散 1200ms → 停顿 120ms → 目标块 0→1 重生 900ms
//
// 为何合入要分两段且中间停顿：同时放会读成「一个变小一个变大」，
// 看不出「两者变成了一体」。先看不到源、再看到目标从头冒出，因果关系才成立。
var SM_GROW_MS = 1800;
var SM_DISSOLVE_MS = 1200;
var SM_MERGE_APPEAR_DELAY = 120; // 源消失→目标重生 的停顿
var SM_MERGE_GROW_MS = 900;

// starmapStartGrow 让一批块从 0 弹到正常大小（新插入）。
function starmapStartGrow(ids) {
	var idx = starmapMeshIndex();
	for (var i = 0; i < ids.length; i++) {
		var m = idx[ids[i]];
		if (!m) continue;
		m.userData.growAt = Date.now();
		starmapMorphAdd(m); // 注册到形变活跃集（否则 tick 不会推进它）
	}
}

// starmapStartDissolve 让一批块消散（删除/被合入）。
//
// 实现：把 mesh 从主场景摘下来，改挂到「消散区」自己推进 —— 而不是原地缩小。
// 若只缩小而不摘，在 1200ms 里它仍参与鼠标拾取与脉冲命中，
// 用户会发“点了一个正在消失的节点”这种怪事。
function starmapStartDissolve(ids, delayMs) {
	var idx = starmapMeshIndex();
	var now = Date.now();
	for (var i = 0; i < ids.length; i++) {
		var m = idx[ids[i]];
		if (!m) continue;
		if (m.userData.dissolving) continue; // 已在消散中，不重复触发（否则粒子叠倍）
		m.userData.dissolving = true;
		m.userData.dissolveAt = now + (delayMs || 0);
		m.userData.dissolveEnds = now + (delayMs || 0) + SM_DISSOLVE_MS;
		m.userData.dissolveFrom = m.scale.x;
		starmapMorphAdd(m); // 注册到形变活跃集
		starmapSpawnParticles(m);
	}
}

// starmapStartMerge 播「两节点消失又出现」。
function starmapStartMerge(fromID, toID) {
	var idx = starmapMeshIndex();
	// ① 源块先消散
	starmapStartDissolve([fromID], 0);
	// ② 目标块在停顿之后重生（不是立即）
	var dst = idx[toID];
	if (dst) {
		var delayed = Date.now() + SM_DISSOLVE_MS + SM_MERGE_APPEAR_DELAY;
		var timer = setTimeout(() => {
			if (dst && dst.parent) {
				dst.userData.growAt = Date.now();
				starmapMorphAdd(dst); // 重生也要注册
			}
		}, SM_DISSOLVE_MS + SM_MERGE_APPEAR_DELAY);
		if (state.starmapMergeTimers) state.starmapMergeTimers.push(timer);
		else state.starmapMergeTimers = [timer];
		// 记录重生时刻，供 tick 区分“合入重生”与普通生长（前者更短）
		dst.userData.mergeGrowAt = delayed;
	}
}

// 粒子池：消散动画的碎屑。用**一个** THREE.Points 承载全部粒子，
// 而不是每粒子一个 mesh —— 后者在批量删除（如 Purge 上百块）时会瞬间建上千个对象。
var starmapParticles = null;
var starmapParticleData = []; // {pos:Vector3, vel:Vector3, born, life}
var SM_PARTICLE_MAX = 600;

// starmapSpawnParticles 在某个节点位置生成一圈飞散粒子。
function starmapSpawnParticles(mesh) {
	if (!starmapScene) return;
	var n = 10;
	for (var i = 0; i < n && starmapParticleData.length < SM_PARTICLE_MAX; i++) {
		// 球面随机方向（均匀分布，不是立方体随机 —— 后者会在角上堆积）。
		var th = Math.random() * Math.PI * 2,
			ph = Math.acos(2 * Math.random() - 1);
		var sp = 0.35 + Math.random() * 0.5;
		starmapParticleData.push({
			pos: mesh.position.clone(),
			vel: new THREE.Vector3(
				Math.sin(ph) * Math.cos(th) * sp,
				Math.sin(ph) * Math.sin(th) * sp,
				Math.cos(ph) * sp,
			),
			born: Date.now(),
			life: 700 + Math.random() * 500,
			color: mesh.material.color
				? mesh.material.color.clone()
				: new THREE.Color(0x8b9dc4),
		});
	}
	starmapEnsureParticleSystem();
}

function starmapEnsureParticleSystem() {
	if (starmapParticles) return;
	var geo = new THREE.BufferGeometry();
	var cap = SM_PARTICLE_MAX;
	geo.setAttribute(
		"position",
		new THREE.BufferAttribute(new Float32Array(cap * 3), 3),
	);
	geo.setAttribute(
		"color",
		new THREE.BufferAttribute(new Float32Array(cap * 3), 3),
	);
	var mat = new THREE.PointsMaterial({
		size: 0.6,
		vertexColors: true,
		transparent: true,
		opacity: 0.9,
		sizeAttenuation: true,
		blending: THREE.AdditiveBlending,
	});
	starmapParticles = new THREE.Points(geo, mat);
	starmapParticles.frustumCulled = false; // 粒子分布很广，包围盒判据会误剪
	starmapParticles.renderOrder = 3;
	starmapScene.add(starmapParticles);
}

// starmapTickParticles 推进粒子。返回活跃粒子数（0 时可整块隐藏）。
function starmapTickParticles(now) {
	if (!starmapParticles || !starmapParticleData.length) {
		if (starmapParticles) starmapParticles.visible = false;
		return 0;
	}
	var live = [];
	for (var i = 0; i < starmapParticleData.length; i++) {
		var p = starmapParticleData[i];
		if (now - p.born > p.life) continue;
		live.push(p);
	}
	starmapParticleData = live;
	var posAttr = starmapParticles.geometry.getAttribute("position");
	var colAttr = starmapParticles.geometry.getAttribute("color");
	for (var j = 0; j < live.length; j++) {
		var q = live[j];
		var age = (now - q.born) / q.life;
		// 减速飘散：速度随时间衰减，看着像“碎屑被拖住”而不是匀速飞走。
		q.pos.x += q.vel.x * (1 - age) * 0.5;
		q.pos.y += q.vel.y * (1 - age) * 0.5;
		q.pos.z += q.vel.z * (1 - age) * 0.5;
		posAttr.setXYZ(j, q.pos.x, q.pos.y, q.pos.z);
		// 淡出：亮度随 age 衰减
		var f = Math.max(0, 1 - age);
		colAttr.setXYZ(j, q.color.r * f, q.color.g * f, q.color.b * f);
	}
	// 未用到的槽位置到远处，避免上一帧的残留粒子“卡”在原地。
	for (var k = live.length; k < SM_PARTICLE_MAX; k++)
		posAttr.setXYZ(k, 1e6, 1e6, 1e6);
	posAttr.needsUpdate = true;
	colAttr.needsUpdate = true;
	starmapParticles.geometry.setDrawRange(0, live.length);
	starmapParticles.visible = live.length > 0;
	return live.length;
}

// starmapPulseByIDs 按**真实块 ID** 高亮（memory_access 事件驱动）。
//
// ★ 为何必须新建这条路径：既有的 starmapPulse(kind, hint) 靠**工具名猜节点**
//   （把 "memory_recall" 拆词去匹配实体名）。而实体名是中文概念、工具名是英文，
//   两套命名空间永不交集 —— 生产实测每个工具名命中 0 个节点，于是每次都退回
//   「按 mention_count 取前 8 个」，即**无论 agent 在干什么，亮起的都是同一批**。
//   后端现在直接告诉内核「刚才读了/写了这几块」（见 events.EventMemoryAccess），
//   这里按 ID 精确命中，反馈才真的与动作对应。
//
// ids 为块 ID 数组；kind 决定动画类型（tool/grow/output）。
// 返回命中的节点数（供调用方决定要不要走旧的 hint 兜底）。
function starmapPulseByIDs(ids, kind) {
	if (!starmapScene || !ids || !ids.length) return 0;
	var now = Date.now();
	state.starmapLastPulseAt = now;
	var hit = 0;
	var life = kind === "grow" ? SM_PULSE_MS * 1.6 : SM_PULSE_MS;
	// 块 ID 直查：建一张 id→mesh 索引，避免每个 ID 都线性扫 3000 个节点。
	var idx = starmapMeshIndex();
	for (var i = 0; i < ids.length; i++) {
		var m = idx[ids[i]];
		if (!m) continue; // 该块不在当前视图（可能刚建、下次全量刷新才出现）
		hit++;
		state.starmapPulses.push({ mesh: m, until: now + life, kind: kind });
	}
	if (state.starmapPulses.length > 260)
		state.starmapPulses = state.starmapPulses.slice(-260);
	return hit;
}

// starmapMeshIndex 返回 nodeId → mesh 的索引（惰性重建，建图后失效）。
var starmapMeshIdx = null;
var starmapMeshIdxSize = -1;
function starmapMeshIndex() {
	if (starmapMeshIdx && starmapMeshIdxSize === starmapNodeMeshes.length)
		return starmapMeshIdx;
	var idx = {};
	for (var i = 0; i < starmapNodeMeshes.length; i++) {
		idx[starmapNodeMeshes[i].userData.nodeId] = starmapNodeMeshes[i];
	}
	starmapMeshIdx = idx;
	starmapMeshIdxSize = starmapNodeMeshes.length;
	return idx;
}

// smOnMemoryAccess 处理 memory_access SSE 事件：真实图读写 → 星图动画。
//
// 事件载荷（见 internal/events/bus.go 的 EventMemoryAccess）：
//
//	op:      recall | commit | merge | purge | delete
//	blocks:  本次读/写的块 ID
//	created: 本次**新建**的块 ID  → 生长动画（从 0 弹到正常大小）
//	removed: 被删除/合入消失的块 → 粒子消散
//	merged:  [{from,to}] 合入对   → 两节点消失又出现
function smOnMemoryAccess(ev) {
	var p = ev.payload || {};
	var op = p.op || "";
	var blocks = p.blocks || [];
	var created = p.created || [];
	var removed = p.removed || [];
	var merged = p.merged || [];

	// 写入/读取都算「被触碰」⇒ 实色提亮（output 类脉冲更强）。
	if (blocks.length) {
		var kind = op === "commit" ? "output" : "tool";
		starmapPulseByIDs(blocks, kind);
	}

	// 新建 ⇒ 生长动画。单独处理而不复用 pulse：生长的时长与曲线不同
	// （见 starmapTickActivity 的 kind === "grow" 分支），混在一起会失去区分。
	if (created.length) {
		starmapStartGrow(created);
	}

	// 删除 ⇒ 粒子消散。
	if (removed.length) {
		starmapStartDissolve(removed);
	}

	// 合入 ⇒ 两节点消失又出现。
	//
	// 时序（这是“又出现”的要点，不能与消散同时放）：
	//   t=0        源块开始消散
	//   t=DISSOLVE 源块消失；目标块开始「再生长」（从小弹回原大小）
	//   t=DISSOLVE+GROW 完成
	// 若两者同时放，看上去只是「一个变小一个变大」，读不出「合入」的因果。
	if (merged.length) {
		for (var i = 0; i < merged.length; i++) {
			starmapStartMerge(merged[i].from, merged[i].to);
		}
	}

	// 图结构变了：下次刷新重拉全量，让新块/消失的块真正进出图。
	if (created.length || removed.length || merged.length) {
		starmapDirty = true;
	}
}

function starmapPulse(kind, hint) {
	if (!starmapScene || !starmapNodeMeshes.length) return;
	var now = Date.now();
	state.starmapLastPulseAt = now;
	// 有 hint（工具名/阶段名）时优先点亮名字与提示相关的节点，
	// 这是「图在跟 agent 动」最直接的体现：调了 knowledge_* 就亮知识节点。
	var targets = starmapPickPulseTargets(hint);
	if (!targets.length) return;
	var life = kind === "grow" ? SM_PULSE_MS * 1.6 : SM_PULSE_MS;
	targets.forEach((m) => {
		state.starmapPulses.push({ mesh: m, until: now + life, kind: kind });
	});
	// 队列上限：防止密集工具调用时脉冲无限堆积占内存。
	if (state.starmapPulses.length > 260)
		state.starmapPulses = state.starmapPulses.slice(-260);
}

// starmapPickPulseTargets 选出该被点亮的节点。
// 优先名字/类型命中 hint 的；不足时按 mention_count 补齐（高权重节点
// 本身就是最常被 agent 触碰的，用它们代表「整体活动」合理）。
//
// ★ 匹配必须用「词」而不是子串包含：
//   hint="knowledge" 与实体名 "k" / "e" 互为子串，会让半个图谱
//   （含 "时"、"会" 这类单字实体）全部命中 ⇒ 脉冲退化成「全图齐亮」，
//   既看不出关联，又把队列瞬间打满。浏览器实测：旧写法一次 pulse
//   就选中 250 个节点、队列顶到 260 上限。
//
// ★★ 2026-10-13：hint 为空的「补齐」分支要全量排序 3754 项，而它每个
//    SSE 事件都跑一次。实测（CDP Profiler 稳态）是纯 JS 里最贵的一项。
//   按 hint 缓存结果，并在建图时失效 —— 补齐结果只依赖于图数据与 hint。
function starmapPickPulseTargets(hint) {
	var out = [];
	if (!starmapNodeMeshes.length) return out;
	var ck = starmapLabelBaseVer + "|" + String(hint || "");
	if (starmapPulseTargetCache[ck]) return starmapPulseTargetCache[ck];
	if (hint) {
		var keys = starmapHintTokens(hint);
		if (keys.length) {
			for (var i = 0; i < starmapNodeMeshes.length && out.length < 26; i++) {
				var nd = starmapNodeMeshes[i].userData.nodeData || {};
				var nm = String(nd.name || "").toLowerCase();
				var ty = String(nd.type || "").toLowerCase();
				if (!nm) continue;
				for (var k = 0; k < keys.length; k++) {
					var key = keys[k];
					// 词边界命中：实体名恰好等于该词，或以该词为词首
					// （如 "knowledge_base" 命中词 "knowledge"）。
					if (nm === key || nm.indexOf(key + "_") === 0 || ty === key) {
						out.push(starmapNodeMeshes[i]);
						break;
					}
				}
			}
		}
	}
	// ★ 只有「一个都没匹配上」时才用全局权重节点代表「整体活动」。
	//   实测（浏览器里跑真数据）：词匹配对 knowledge_list 只命中 1 个，
	//   但旧的补齐逻辑会把它补到 20 个 —— 于是脉冲看起来仍然是「一大片
	//   无关节点在亮」，与要修的子串 bug 效果一样，只是换了个成因。
	//   补齐只保留在真正无匹配的场景（hint 为空，如调度器脉冲）。
	if (out.length > 0) {
		starmapPulseTargetCache[ck] = out;
		return out;
	}
	// 补齐：按 mention_count 降序取靠前且尚未入列的。
	// 排序结果按 baseVer 缓存复用（图不变则排序不变）。
	var sorted = starmapMeshByWeight;
	if (sorted === null || starmapMeshByWeightVer !== starmapLabelBaseVer) {
		sorted = starmapNodeMeshes
			.slice()
			.sort(
				(a, b) =>
					((b.userData.nodeData || {}).mention_count || 0) -
					((a.userData.nodeData || {}).mention_count || 0),
			);
		starmapMeshByWeight = sorted;
		starmapMeshByWeightVer = starmapLabelBaseVer;
	}
	for (var j = 0; j < sorted.length && out.length < 8; j++) {
		if (out.indexOf(sorted[j]) === -1) out.push(sorted[j]);
	}
	starmapPulseTargetCache[ck] = out;
	return out;
}

// starmapHintTokens 把提示词（工具名/阶段名）拆成可匹配的词元。
// 例："knowledge_list" → ["knowledge","list"]。
// 只保留长度 >= 3 的词：单/双字母词（"a"/"ls"）几乎必然撞上无关
// 实体名，匹配它们只会制造噪声。
function starmapHintTokens(hint) {
	var raw = String(hint)
		.toLowerCase()
		.split(/[^a-z0-9\u4e00-\u9fa5]+/);
	var out = [];
	for (var i = 0; i < raw.length; i++) {
		var t = raw[i];
		if (t.length >= 3 && out.indexOf(t) === -1) out.push(t);
	}
	return out;
}

// starmapPullActivity 拉 /runtime，把调度器状态映射成图的整体节奏。
// starmapPullActivity 用**已有的** /runtime 快照驱动图，绝不自己再发一轮请求。
//
// ★ 原来它直接 api("/runtime")，而 startRuntimeTicker 也在 3s 拉同一个
//   接口 —— 浏览器实测：停在总览页 40s 内 /runtime 被打 **17 次**
//   （= 3s 一次 × 2 + 15s 的 renderAll）。两个消费者拉同一份数据是纯浪费。
//
// 现在只读 state.runtime（由 startRuntimeTicker / renderAll 负责刷新）。
// 星图慢一拍（最多 3s）无所谓：它只是把调度器状态映射成脉冲。
function starmapPullActivity() {
	if (!starmapScene) return;
	var s0 = state.runtime && state.runtime.scheduler;
	if (!s0) {
		// 还没有快照（星图先于运行态轮询初始化）：拉一次，建立基线。
		starmapFetchBlock("runtime").then(() => {
			starmapApplyActivity();
		});
		return;
	}
	starmapApplyActivity();
}

// starmapApplyActivity 把 state.runtime 里的调度器计数映射成星图脉冲。
function starmapApplyActivity() {
	var rt = state.runtime;
	if (!rt || !rt.scheduler) return;
	var s = rt.scheduler;
	var prev = state.starmapActivity;
	state.starmapActivity = s;
	var pend =
		(s.ready_queue_depth || 0) +
		(s.pending_interrupts || 0) +
		(s.suspend_stack || 0);
	// 有任务在排队/中断 ⇒ 脉冲，让图「绷紧」。
	if (pend > 0) starmapPulse("stage", null);
	if (!prev) {
		smUpdateStat();
		return;
	}
	// 中断或抢占计数上升 ⇒ 一次强脉冲（高优先级插入）。
	var sum = (a) => (a || []).reduce((x, y) => x + y, 0);
	if (
		sum(s.interrupts_by_level) - sum(prev.interrupts_by_level) > 0 ||
		sum(s.preempts_by_level) - sum(prev.preempts_by_level) > 0
	) {
		starmapPulse("output", null);
	}
	smUpdateStat();
}

// starmapPullPulse 拉轻量活动端点，把新长出来的节点标记为「生长」。
function starmapPullPulse() {
	if (!starmapScene) return;
	var since = state.starmapPulseSince || 0;
	api("/memory/graph/pulse?since=" + since)
		.then((r) => {
			if (!r || !r.success || !r.data) return;
			var now = Math.floor(Date.now() / 1000);
			state.starmapPulseSince = now - 30; // 30s 重叠，防跨轮漏节点
			var nodes = r.data.nodes || [];
			if (!nodes.length) return;
			var known = 0;
			nodes.forEach((n) => {
				var m = starmapNodeMeshes.find((x) => x.userData.nodeId === n.id);
				if (!m) return; // 全量图里没有（可能刚创建）⇒ 忽略，等下次全量
				known++;
				state.starmapGrown[m.userData.nodeId] = Date.now() + SM_PULSE_MS * 2;
				starmapPulse("grow", n.name);
			});
			// 若有新实体但一个都没匹配上，说明全量图过期了，
			// 下一拍重拉全量（新节点才能出现）。
			if (known === 0 && nodes.length > 2) starmapDirty = true;
		})
		.catch(() => {});
}

// starmapTickActivity 在渲染循环里推进所有脉冲与余晖。
// starmapTickMorph 推进「生长」与「消散」两种形变。
//
// ★ 为何不用已有的 pulse 机制做这两件事：pulse 是**可叠加的短脉冲**
//   （同一个节点可以同时有好几个），而生长/消散是**排他的状态机**
//   （一个节点不可能同时在长又在消）。混用会让基值互相覆盖，
//   出现「消散到一半被 pulse 拉回原大小」的鬼畜。
var starmapMorphSet = null; // Set<mesh>，懒建

// starmapMorphAdd 把一个节点加入形变活跃集（生长/消散都要）。
function starmapMorphAdd(mesh) {
	if (!starmapMorphSet) starmapMorphSet = new Set();
	starmapMorphSet.add(mesh);
}

// starmapTickMorph 推进「生长」与「消散」两种形变。
//
// ★ 性能（2026-10-08 修「非常卡」）：原先每帧无条件扫全部节点，
//   生产库 3190 块 × 60fps ⇒ 每秒 19 万次 userData 读取与分支，
//   而绝大多数帧里「正在形变」的节点是 0 个。
//   改用 starmapMorphSet 记录活跃节点，空集合时 O(1) 返回。
function starmapTickMorph(now) {
	if (!starmapMorphSet || starmapMorphSet.size === 0) return; // 常见路径：零开销
	var done = [];
	starmapMorphSet.forEach((m) => {
		var ud = m.userData;

		// —— 消散 ——
		if (ud.dissolving) {
			if (now < ud.dissolveAt) return; // 合入时源块的延迟（若有）
			var dp = (now - ud.dissolveAt) / SM_DISSOLVE_MS;
			if (dp >= 1) {
				// 完了：必须真的从场景摘掉，否则一个 scale=0 的不可见 mesh
				// 仍参与鼠标拾取，用户会“点中一个看不见的节点”。
				starmapScene.remove(m);
				ud.dissolving = false;
				done.push(m);
				return;
			}
			// 收缩 + 变暗：两个通道同时走，单个通道（只缩）看着像“缩成点”，
			// 加上变暗才有“消失”的观感。
			var shrink = 1 - dp * dp; // 加速收缩（先慢后快）
			m.scale.setScalar(ud.baseScale * shrink);
			if (m.material) m.material.emissiveIntensity = ud.baseEmissive * (1 - dp);
			return;
		}

		// —— 生长（插入 / 合入后重生）——
		if (ud.growAt) {
			var dur =
				ud.mergeGrowAt && ud.mergeGrowAt === ud.growAt
					? SM_MERGE_GROW_MS
					: SM_GROW_MS;
			var gp = (now - ud.growAt) / dur;
			if (gp >= 1) {
				ud.growAt = null;
				ud.mergeGrowAt = null;
				m.scale.setScalar(ud.baseScale);
				if (m.material) m.material.emissiveIntensity = ud.baseEmissive;
				done.push(m);
				return;
			}
			// 过冲曲线（back-out）：超过 1 再回落 —— 这才像“弹”而不是“淡入”。
			var s = 1.7;
			var t1 = gp - 1;
			var eased = t1 * t1 * ((s + 1) * t1 + s) + 1; // easeOutBack
			// 从 0.05 而不是 0 起：从真正的 0 开始会看不到“从无到有”的那一瞬，
			// 视觉上像是“凭空出现”而非“长出来”。
			var scale = 0.05 + 0.95 * Math.max(0, eased);
			m.scale.setScalar(ud.baseScale * scale);
			if (m.material)
				m.material.emissiveIntensity = ud.baseEmissive + 0.9 * (1 - gp);
		}
	});
	// 已完成的从活跃集移除（本次遍历后，不做边遍历边删）。
	for (var i = 0; i < done.length; i++) starmapMorphSet.delete(done[i]);
}

function starmapTickActivity(t) {
	var now = Date.now();
	var breathe = 0;
	// 1) 活动脉冲：发光冲高 + 尺寸微扩 + 涟漪环
	if (state.starmapPulses.length) {
		var keep = [];
		for (var i = 0; i < state.starmapPulses.length; i++) {
			var p = state.starmapPulses[i];
			if (p.until <= now) {
				p.mesh.material.emissiveIntensity = p.mesh.userData.baseEmissive;
				p.mesh.scale.setScalar(p.mesh.userData.baseScale);
				continue;
			}
			keep.push(p);
			var left = (p.until - now) / SM_PULSE_MS; // 1→0
			var k = 1 - left; // 0→1
			var wave = Math.sin(Math.min(1, k) * Math.PI);
			var amp = p.kind === "output" ? 1.8 : 1.2;
			p.mesh.material.emissiveIntensity =
				p.mesh.userData.baseEmissive + wave * amp;
			p.mesh.scale.setScalar(p.mesh.userData.baseScale * (1 + wave * 0.28));
			// 生长：新节点从 0 弹到正常大小
			if (p.kind === "grow") {
				var g = Math.min(1, k * 1.4);
				p.mesh.scale.setScalar(p.mesh.userData.baseScale * (0.15 + 0.85 * g));
			}
		}
		state.starmapPulses = keep;
	}
	// 1.5) 生长 / 消散 / 合入（图变化动画）
	//
	// 放在活动脉冲之后、呼吸之前：图变化是**结构性**事件，优先级高于活动光晕，
	// 而被呼吸的抽样提亮覆盖会看不出“长出来”。
	starmapTickMorph(now);
	starmapTickParticles(now);

	// 2) 「生长」余晖：脉冲结束后短暂保留一点亮
	if (state.starmapGrown) {
		for (var gid in state.starmapGrown) {
			if (state.starmapGrown[gid] <= now) {
				delete state.starmapGrown[gid];
				continue;
			}
			var gm = starmapNodeMeshes.find((x) => x.userData.nodeId == gid);
			if (gm)
				gm.material.emissiveIntensity = Math.max(
					gm.material.emissiveIntensity,
					gm.userData.baseEmissive + 0.6,
				);
		}
	}
	// 3) 全局呼吸：距上次活动越近越亮，实现「agent 一忙图就活」
	var idle = (now - (state.starmapLastPulseAt || 0)) / 4000;
	breathe = Math.max(0, 1 - idle);
	if (breathe > 0.01 && starmapNodeMeshes.length) {
		// 只抽样一部分节点做呼吸，避免每帧改 1151 个材质。
		var stride = 24;
		for (var b = 0; b < starmapNodeMeshes.length; b += stride) {
			var m2 = starmapNodeMeshes[b];
			if (m2.userData.baseEmissive === undefined) continue;
			m2.material.emissiveIntensity = Math.max(
				m2.userData.baseEmissive,
				m2.userData.baseEmissive + breathe * 0.25,
			);
		}
	}
	return breathe;
}

function createStarField() {
	var c = 3000;
	var p = new Float32Array(c * 3),
		cl = new Float32Array(c * 3),
		s = new Float32Array(c);
	for (var i = 0; i < c; i++) {
		var i3 = i * 3;
		var r = 400 + Math.random() * 600,
			th = Math.random() * Math.PI * 2,
			ph = Math.acos(2 * Math.random() - 1);
		p[i3] = r * Math.sin(ph) * Math.cos(th);
		p[i3 + 1] = r * Math.sin(ph) * Math.sin(th);
		p[i3 + 2] = r * Math.cos(ph);
		if (Math.random() < 0.7) {
			cl[i3] = 0.8 + Math.random() * 0.2;
			cl[i3 + 1] = 0.8 + Math.random() * 0.2;
			cl[i3 + 2] = 1;
		} else {
			cl[i3] = 1;
			cl[i3 + 1] = 0.9 + Math.random() * 0.1;
			cl[i3 + 2] = 0.8 + Math.random() * 0.2;
		}
		s[i] = 0.5 + Math.random() * 2;
	}
	var g = new THREE.BufferGeometry();
	g.setAttribute("position", new THREE.BufferAttribute(p, 3));
	g.setAttribute("color", new THREE.BufferAttribute(cl, 3));
	g.setAttribute("size", new THREE.BufferAttribute(s, 1));
	var m = new THREE.PointsMaterial({
		size: 1.5,
		vertexColors: true,
		transparent: true,
		opacity: 0.8,
		sizeAttenuation: true,
	});
	starmapStarField = new THREE.Points(g, m);
	starmapScene.add(starmapStarField);
}

function onStarmapMove(e) {
	if (!starmapRen || !starmapCam) return;
	var rect = starmapRen.domElement.getBoundingClientRect();
	var mouse = new THREE.Vector2(
		((e.clientX - rect.left) / rect.width) * 2 - 1,
		-((e.clientY - rect.top) / rect.height) * 2 + 1,
	);
	var rc = new THREE.Raycaster();
	rc.setFromCamera(mouse, starmapCam);
	var hits = rc.intersectObjects(starmapNodeMeshes);
	if (hits.length > 0) {
		var n = hits[0].object;
		if (starmapHovered !== n) {
			// 复位用 baseScale，不能用 set(1,1,1)：节点现在是按 mention_count
			// 缩放过的（userData.baseScale），置 1 会把大节点缩成最小尺寸。
			// （这是低规格改造后必须跟着改的一处，旧代码能“跑”是因为那时
			//  几何体本身就带半径、不靠 scale。）
			if (starmapHovered) {
				starmapHovered.scale.setScalar(starmapHovered.userData.baseScale || 1);
				if (starmapHovered.material)
					starmapHovered.material.emissiveIntensity =
						starmapHovered.userData.baseEmissive;
			}
			starmapHovered = n;
			// 悬浮「提亮」：加发光 + 轻微放大。只放大不亮在淡色底图上不明显，
			// 而只亮不放大又难看出是哪一颗——两者一起做。
			if (n.material)
				n.material.emissiveIntensity = (n.userData.baseEmissive || 0) + 1.4;
			n.scale.setScalar((n.userData.baseScale || 1) * 1.25);
			starmapLabelShow(n);
		}
	} else {
		if (starmapHovered) {
			starmapHovered.scale.setScalar(starmapHovered.userData.baseScale || 1);
			if (starmapHovered.material)
				starmapHovered.material.emissiveIntensity =
					starmapHovered.userData.baseEmissive;
			starmapHovered = null;
			starmapLabelHide();
		}
	}
}

// starmapLabelShow/Hide：hover 时在容器角上显示一个 HTML 角标。
// 取代原先每节点一张 256x64 CanvasTexture（1151 张贴图 ≈ 72MB 显存），
// 文字用 DOM 渲染反而更清楚，且零 GPU 开销。
function starmapLabelShow(mesh) {
	var cont = starmapActiveContainer();
	if (!cont) return;
	if (!starmapLabelEl) {
		starmapLabelEl = document.createElement("div");
		starmapLabelEl.id = "sm-label";
	}
	// 角标必须跟 canvas 在同一个容器里（它是绝对定位在容器上的）。
	// canvas 换页签搬运时，角标也要跟着搬。
	if (starmapLabelEl.parentElement !== cont) cont.appendChild(starmapLabelEl);
	var nd = (mesh && mesh.userData && mesh.userData.nodeData) || {};
	var nm = nd.name || nd.id || "";
	starmapLabelEl.textContent =
		nm + (nd.mention_count ? "  ×" + nd.mention_count : "");
	starmapLabelEl.style.display = "block";
}
function starmapLabelHide() {
	if (starmapLabelEl) starmapLabelEl.style.display = "none";
}

// starmapActiveContainer 选出「该把星图画在哪」的容器。
//
// 规则：先看独立星图页签，不存在（该页签还没打开过）就退回总览页，
// 再退回聊天面板。为什么要这个优先级：three.js 的 canvas 只能有一个
// 父节点，同时往两处渲染就会一边黑屏。每次切页签都把 canvas 搬到
// 当前该显示的地方，是单一 renderer 前提下最干净的做法。
function starmapActiveContainer() {
	var tab = document.getElementById("tab-starmap");
	if (tab && tab.classList.contains("active")) {
		var c = document.getElementById("sm-container-page");
		if (c) return c;
	}
	var ov = document.getElementById("tab-overview");
	if (ov && ov.classList.contains("active")) {
		var h = document.getElementById("sm-container-home");
		if (h) return h;
	}
	return (
		document.getElementById("sm-container-chat") ||
		document.getElementById("sm-container-home") ||
		null
	);
}

// renderStarmapTab 渲染独立星图页签（只建一次骨架，不重复重建）。
function renderStarmapTab() {
	var host = document.getElementById("tab-starmap");
	if (!host) return;
	if (!document.getElementById("sm-container-page")) {
		var rt = state.runtime || {};
		var sc = rt.scheduler || {};
		var pend = (sc.ready_queue_depth || 0) + (sc.pending_interrupts || 0);
		host.innerHTML =
			'<div class="card"><h2>' +
			__("记忆星图", "Memory Star Map") +
			' <span class="badge" id="sm-page-stat" style="font-size:10px;font-weight:400"></span></h2>' +
			'<div id="sm-container-page" style="height:calc(100vh - 260px);min-height:420px"></div>' +
			'<div id="sm-legend" style="display:flex;gap:14px;flex-wrap:wrap;margin-top:10px;font-size:11px;color:var(--text-secondary)"></div>' +
			'<div style="margin-top:8px;font-size:11px;color:var(--text-muted)">' +
			__(
				"星图显示图记忆的实时状态：大小/亮度 = 被引用程度；agent 读写记忆时对应块会提亮（按真实块 ID，不靠名字猜）；新增记忆生长、删除消散、合入则源块消失、目标重生。点节点看详情。",
				"Live graph memory: size/brightness = how referenced; blocks glow when the agent reads/writes them (by real block ID, not name guessing); new memories grow, deletions dissolve, merges fade the source and rebirth the target. Click a node for details.",
			) +
			"</div></div>";
	}
	// 页签每次激活都把 canvas 搬过来 + 重新按容器尺寸 resize。
	if (document.getElementById("sm-container-page")) {
		if (!state.starmapInit && !state.starmapLoading) loadChatStarmapData();
		else if (starmapRen) {
			var cont = document.getElementById("sm-container-page");
			if (starmapRen.domElement.parentElement !== cont) {
				cont.appendChild(starmapRen.domElement);
				starmapRen.domElement.style.display = "block";
			}
			onStarmapResize();
		}
	}
	// ★ 图例也要在这里重算，而不只是 initChatStarmap 里算一次：
	//   首屏时星图是在**总览页**初始化的（那时 #sm-legend 还不存在，
	//   smLegend 内部 getElementById 返回 null 直接返回），而本函数的
	//   容器骨架恰恰是此刻才建的。不在这里补一次，图例就永远是空的。
	smLegend();
	smUpdateStat();
}

// smLegendLabel 把内部类型名转成可读标签。
var SM_TYPE_LABELS = {
	person: ["人物", "Person"],
	concept: ["概念", "Concept"],
	object: ["对象", "Object"],
	location: ["地点", "Location"],
	source: ["来源", "Source"],
	document: ["文档", "Document"],
	event: ["事件", "Event"],
	entity: ["实体", "Entity"],
	scene: ["场景", "Scene"],
	ai: ["AI", "AI"],
	task: ["任务", "Task"],
};

// smLegend 按**实际出现**的类型生成图例。
//
// ★ 为什么不用固定列表：原图例写死了「人物/概念/对象/地点/来源」五项，
// 而实测图谱里只有 Concept（1148）与 Source（1）。列出四个永不出现的
// 类型 = 对用户说谎（图例说“分类”，实际是一整片同色）。
//
// 现在：
//   - 只有一种类型  → 只显示一项，并标注占比（如「概念 1149」）
//   - 多种类型      → 按数量降序列出全部，每项带占比
//   - 类型名不认识  → 退化为小写原文而不是默默消失
function smLegend() {
	var box = document.getElementById("sm-legend");
	if (!box) return;
	var keys = Object.keys(smPresentTypes);
	if (!keys.length || !starmapNodes.length) {
		box.innerHTML = "";
		return;
	}
	var total = starmapNodes.length;
	keys.sort((a, b) => smPresentTypes[b] - smPresentTypes[a]);

	// ★ 图例改为说明**视觉编码**，而不是列出类型色（2026-10-08）。
	//
	// 为何改：星图已改为灰色调 + 亮度按度数（见 smGreyFor），
	// 再说“类型→颜色”就是骗人的图例——读者会去找不存在的颜色差别。
	// 现在的图例应该回答“图上什么东西怎么看”：大小/亮度/名字/活动。
	var html = [];
	// ① 大小与亮度 = 重要度（度数）
	html.push(
		'<span style="display:inline-flex;align-items:center;gap:5px">' +
			'<i style="width:13px;height:13px;border-radius:50%;background:#dcdfe6;display:inline-block"></i>' +
			__("常被引用（越大越亮）", "most referenced (bigger & brighter)") +
			"</span>",
	);
	html.push(
		'<span style="display:inline-flex;align-items:center;gap:5px">' +
			'<i style="width:7px;height:7px;border-radius:50%;background:#787f89;display:inline-block"></i>' +
			__("孤立/少引用", "isolated / few links") +
			"</span>",
	);
	// ② 活动态：读写命中时会提亮并显示名字
	html.push(
		'<span style="display:inline-flex;align-items:center;gap:5px">' +
			'<i style="width:10px;height:10px;border-radius:50%;background:#fff;box-shadow:0 0 6px #fff;display:inline-block"></i>' +
			__("正在被读写（提亮）", "being read/written (glows)") +
			"</span>",
	);
	// ③ 图变化动画的三种形态
	html.push(
		'<span style="color:var(--text-muted)">' +
			__(
				"＋ 新记忆生长　－ 删除消散　⇄ 合入（源消失、目标重生）",
				"+ grows 　- dissolves 　⇄ merges (source fades, target reborn)",
			) +
			"</span>",
	);
	// ④ 类型构成：保留总览，但**不再附带颜色块**（颜色不再编码类型）
	var minor = 0;
	var major = [];
	keys.forEach((k) => {
		if ((smPresentTypes[k] * 100) / total >= 1) major.push(k);
		else minor += smPresentTypes[k];
	});
	var parts = major.map((k) => {
		var lbl = SM_TYPE_LABELS[k];
		var label = lbl ? __(lbl[0], lbl[1]) : k;
		return (
			escHtml(label) +
			' <span style="color:var(--text-muted)">' +
			Math.round((smPresentTypes[k] * 100) / total) +
			"%</span>"
		);
	});
	if (minor > 0)
		parts.push(__("其他 ", "other ") + minor + __(" 个", " nodes"));
	if (parts.length)
		html.push(
			'<span style="color:var(--text-secondary)">' +
				__("构成：", "composition: ") +
				parts.join(__("、", ", ")) +
				"</span>",
		);
	box.innerHTML = html.join("");
}

// smUpdateStat 在星图页签头部显示节点/边/活动状态。
function smUpdateStat() {
	var el = document.getElementById("sm-page-stat");
	if (!el) return;
	var act = state.starmapActivity;
	var busy = act && (act.ready_queue_depth || act.pending_interrupts);
	var txt = starmapNodes.length + " " + __("节点", "nodes");
	if (starmapEdges.length)
		txt += " / " + starmapEdges.length + " " + __("关系", "edges");
	if (busy) txt += " · " + __("调度中", "busy");
	el.textContent = txt;
}

function onStarmapClick(e) {
	if (!starmapRen || !starmapCam) return;
	var rect = starmapRen.domElement.getBoundingClientRect();
	var mouse = new THREE.Vector2(
		((e.clientX - rect.left) / rect.width) * 2 - 1,
		-((e.clientY - rect.top) / rect.height) * 2 + 1,
	);
	var rc = new THREE.Raycaster();
	rc.setFromCamera(mouse, starmapCam);
	var hits = rc.intersectObjects(starmapNodeMeshes);
	if (hits.length > 0) {
		var n = hits[0].object;
		starmapSelected = starmapSelected === n ? null : n;
		if (starmapAutoView && starmapSelected)
			flyStarmapTo(starmapSelected.userData.nodeId, 500);
		onStarmapMove(e);
		starmapShowDetail(starmapSelected);
	} else {
		starmapSelected = null;
		starmapShowDetail(null);
	}
}

// starmapShowDetail 显示/隐藏选中块的详情面板。
//
// ★ 为何必须补它（2026-10-08）：`starmapSelected` 此前**只被赋值、无人读取**
//   （全仓 grep 确认），于是点击节点只会“飞过去”，看不到它是什么、连了谁——
//   而“可读性差”正是用户反馈的核心。星图既然是一个**图的浏览器**，
//   点一个节点而没有任何信息反馈就不算可用。
//
// 内容取 `userData.nodeData`（smAdaptGraph 归一化后的块信息），
// 并附上它的邻居（从 starmapEdges 现算，不额外请求）。
function starmapShowDetail(mesh) {
	var cont = starmapActiveContainer();
	if (!cont) return;
	var el = document.getElementById("sm-detail");
	if (!mesh) {
		if (el) el.style.display = "none";
		return;
	}
	if (!el) {
		el = document.createElement("div");
		el.id = "sm-detail";
		cont.appendChild(el);
	} else if (el.parentElement !== cont) {
		cont.appendChild(el);
	}
	var nd = mesh.userData.nodeData || {};
	var id = mesh.userData.nodeId;

	// 邻居：从边表现算（最多 12 条，多了也读不过来）。
	var byID = {};
	starmapNodes.forEach((n) => {
		byID[n.id] = n;
	});
	var nb = [];
	for (var i = 0; i < starmapEdges.length && nb.length < 12; i++) {
		var e = starmapEdges[i];
		if (e.source_id === id && byID[e.target_id])
			nb.push({ rel: e.relation_type, name: byID[e.target_id].name, dir: "→" });
		else if (e.target_id === id && byID[e.source_id])
			nb.push({ rel: e.relation_type, name: byID[e.source_id].name, dir: "←" });
	}

	var rows = [];
	rows.push(
		'<div class="sm-detail-title">' + escHtml(String(nd.name || id)) + "</div>",
	);
	rows.push(
		'<div class="sm-detail-meta">' +
			escHtml(String(nd.type || "block")) +
			" · " +
			escHtml(String(nd.source || "-")) +
			(nd.scene ? " · " + escHtml(String(nd.scene)) : "") +
			"</div>",
	);
	if (nb.length) {
		rows.push('<div class="sm-detail-rel">');
		for (var j = 0; j < nb.length; j++) {
			rows.push(
				'<div class="sm-detail-relrow"><span class="sm-detail-arrow">' +
					nb[j].dir +
					"</span>" +
					escHtml(String(nb[j].rel || "关联")) +
					' <span class="sm-detail-nb">' +
					escHtml(String(nb[j].name || "").slice(0, 20)) +
					"</span></div>",
			);
		}
		rows.push("</div>");
	} else {
		rows.push(
			'<div class="sm-detail-rel">' + __("无关联", "no relations") + "</div>",
		);
	}
	rows.push(
		'<div class="sm-detail-id">' + escHtml(String(id).slice(0, 28)) + "</div>",
	);
	el.innerHTML = rows.join("");
	el.style.display = "block";
}

function flyStarmapTo(nodeId, dur) {
	if (!starmapAutoView) return;
	var m = starmapNodeMeshes.find((x) => x.userData.nodeId === nodeId);
	if (!m) return;
	var tp = m.position.clone(),
		sp = starmapCam.position.clone(),
		st = starmapCtrl.target.clone();
	var dist = tp.length() + 25,
		ep = new THREE.Vector3(tp.x, tp.y + dist * 0.4, tp.z + dist * 0.8);
	var t0 = Date.now();
	(function lerp() {
		var t = Math.min((Date.now() - t0) / dur, 1),
			e = 1 - (1 - t) ** 3;
		starmapCam.position.lerpVectors(sp, ep, e);
		starmapCtrl.target.lerpVectors(st, tp, e);
		if (t < 1) requestAnimationFrame(lerp);
	})();
}

function onStarmapResize() {
	if (!starmapRen || !starmapCam) return;
	var cont = starmapRen.domElement.parentElement;
	if (!cont) return;
	var rect = cont.getBoundingClientRect();
	var w = rect.width || 800,
		h = Math.max(rect.height || 250, 100);
	if (w > 0 && h > 0) {
		starmapCam.aspect = w / h;
		starmapCam.updateProjectionMatrix();
		starmapRen.setSize(w, h);
	}
}

// ===== 常驻节点名覆盖层 =====
//
// 用**一个** 2D canvas 把节点名画在 WebGL canvas 之上，而不是每节点一张纹理。
//
// 分三级可读性（这是「全部标名但不糊」的关键）：
//
//  ① 当前被活动命中的块（读写/生长/删除动画中）→ 实色 + 粗体，永远画
//  ② 鼠标悬浮的块 → 实色 + 背景底块，永远画
//  ③ 其余块 → 低透明度当底纹（能看出“这里有个东西、叫什么”），
//     但**同屏去重**：名字空间重叠的只留靠前的那个
//
// 为何要去重而不是全画：3178 个块里有大量同文本块（原句块与实体块同文），
// 全画会在同一个屏幕位置叠上十几层完全相同的字，既看不清又白花 CPU。
function starmapEnsureLabelLayer() {
	var cont = starmapActiveContainer();
	if (!cont || !starmapRen) return null;
	// 容器换了就要重建：canvas 只能有一个父节点。
	if (
		!starmapLabelCanvas ||
		starmapLabelCanvas.parentElement !== cont ||
		starmapLabelCanvas.width !== starmapRen.domElement.width
	) {
		if (starmapLabelCanvas && starmapLabelCanvas.parentElement)
			starmapLabelCanvas.parentElement.removeChild(starmapLabelCanvas);
		var cv = document.createElement("canvas");
		cv.id = "sm-labels";
		// 盖在 WebGL canvas 上，但不吃鼠标事件（否则星图不能拖拽/悬浮）。
		cv.style.position = "absolute";
		cv.style.left = "0";
		cv.style.top = "0";
		cv.style.pointerEvents = "none";
		cv.style.zIndex = "4";
		var w = starmapRen.domElement.width,
			h = starmapRen.domElement.height;
		cv.width = w;
		cv.height = h;
		cv.style.width = starmapRen.domElement.style.width;
		cv.style.height = starmapRen.domElement.style.height;
		cont.appendChild(cv);
		starmapLabelCanvas = cv;
		starmapLabelCtx = cv.getContext("2d");
	}
	return starmapLabelCtx;
}

// starmapMakeLabelPlan 选出本帧要画的标签，返回 [{mesh, text, kind}]。
//
// kind: "hot"（活动命中/悬浮，实色） | "dim"（底纹）。
// 排序按「重要度」：hot 最后画（压在最上层），dim 里 mention_count 高的先画。
function starmapMakeLabelPlan() {
	if (!starmapCam) return [];
	var hot = {};
	// 当前脉冲命中的节点 = hot
	for (var i = 0; i < state.starmapPulses.length; i++) {
		var m = state.starmapPulses[i].mesh;
		if (m) hot[m.userData.nodeId] = true;
	}
	if (starmapHovered) hot[starmapHovered.userData.nodeId] = true;
	if (starmapSelected) hot[starmapSelected.userData.nodeId] = true;

	// ★★ 计划缓存（2026-10-13）。
	//
	// 为何必须缓存：本函数由 starmapDrawLabels 以 10Hz（SM_LABEL_MIN_INTERVAL
	// = 100ms）调用，而每次都要：对 3754 个节点各跑一次正则清洗 + 分配
	// 3754 个对象 + 全量 sort。实测（CDP Profiler，稳态 30s）它是纯 JS 里
	// 最贵的一项（608ms / 2%），并伴大量 GC。
	//
	// 关键观察：plan 只依赖于「mesh 集合」与「hot 集合」。两者在稳态下
	// 都不变（相机移动只影响哪些标签可见，不影响 plan 内容），所以缓存
	// 命中时应当**零工作、零分配**返回上一个数组。
	var hotSig = "";
	for (var hk in hot) hotSig += hk + ",";
	var sig = starmapLabelBaseVer + "|" + hotSig;
	if (starmapLabelPlanCache && starmapLabelPlanSig === sig) {
		return starmapLabelPlanCache;
	}

	var plan = [];
	for (var j = 0; j < starmapNodeMeshes.length; j++) {
		var mesh = starmapNodeMeshes[j];
		// 名字的清洗/截断也一并缓存到 mesh.userData：正则 replace 是
		// 每轮最贵的一步之一，而节点名在两次建图之间不会变。
		var nm = mesh.userData._labelText;
		if (nm === undefined) {
			var nd = mesh.userData.nodeData || {};
			nm = String(nd.name || nd.id || "")
				.replace(/\s+/g, " ")
				.trim();
			// 名字过长时截断：星图是“索引”，长句应该点开看而不是在图上读完。
			if (nm.length > 18) nm = nm.slice(0, 17) + "…";
			mesh.userData._labelText = nm;
		}
		if (!nm) continue;
		plan.push({
			mesh: mesh,
			text: nm,
			kind: hot[mesh.userData.nodeId] ? "hot" : "dim",
			mc: (mesh.userData.nodeData || {}).mention_count || 0,
		});
	}
	// dim 按权重降序（重要的先占位），hot 一律排到最后。
	plan.sort((a, b) => {
		if (a.kind !== b.kind) return a.kind === "hot" ? 1 : -1;
		return b.mc - a.mc;
	});
	starmapLabelPlanCache = plan;
	starmapLabelPlanSig = sig;
	return plan;
}

// starmapDrawLabels 把节点名画到覆盖层。每帧调用，但只在有变化时才重绘。
//
// ★ 性能：3178 节点 × 每帧 project() 会白白烧掉大部分帧。两道剪裁：
//   ① 只在**视角变了**（相机/控件移动）或**脉冲集合变了**时重绘；
//   ② 每个标签先做屏幕坐标与视锥检查，背面/屏外的直接跳过。
var starmapLabelSig = "";
var starmapLabelLastCamKey = "";
// starmapLabelLastDraw：标签层上次重绘时刻（节流用）。
//
// 为何需要节流：OrbitControls 带阻尼，拖动/缩放时相机位置**每帧都在变**，
// 于是 camKey 每帧不同 ⇒ 原先会每帧重建 3190 项的 plan 并对每个可见标签
// 调 measureText（文字度量是同步布局，很贵）。标签是文字层，100ms 刷新
// 肉眼已看不出滞后，而 3D 图形仍是 60fps。
var starmapLabelLastDraw = 0;
var SM_LABEL_MIN_INTERVAL = 100;
// starmapTextWidth：measureText 结果缓存（键 = 字体 + 文字）。
//
// 为何要缓存：measureText 触发同步文字布局，每帧对上千个标签调用会卡主线程。
// 同一文字在同字体下的宽度是确定的，缓存长期有效（字体模板只有两个）。
// 上限保护：避免极端情况下无界增长（超出时整表清空重填）。
var starmapTextWidth = {};
var SM_TEXTWIDTH_MAX = 20000;
// 标签计划缓存（见 starmapMakeLabelPlan）。
//
// baseVer 在每次建图（mesh 集合变化）时自增 —— 它是缓存失效的唯一信号，
// 与 hot 集合的签名拼成一个完整的 cache key。
var starmapLabelPlanCache = null,
	starmapLabelPlanSig = "",
	starmapLabelBaseVer = 0;
// 脉冲目标缓存：hint → mesh 列表。按 baseVer 失效（见 starmapPickPulseTargets）。
var starmapPulseTargetCache = {};
var starmapMeshByWeight = null,
	starmapMeshByWeightVer = -1;

function starmapDrawLabels() {
	if (!starmapRen || !starmapCam || !starmapNodeMeshes.length) {
		if (starmapLabelCtx && starmapLabelCanvas) {
			starmapLabelCtx.clearRect(
				0,
				0,
				starmapLabelCanvas.width,
				starmapLabelCanvas.height,
			);
		}
		return;
	}
	var ctx = starmapEnsureLabelLayer();
	if (!ctx) return;

	var cw = starmapLabelCanvas.width,
		ch = starmapLabelCanvas.height;

	// 视觉签名：相机位置/朝向 + 脉冲条数。两者都没变就不重绘。
	var camKey =
		starmapCam.position.x.toFixed(2) +
		"," +
		starmapCam.position.y.toFixed(2) +
		"," +
		starmapCam.position.z.toFixed(2) +
		"," +
		starmapCtrl.target.x.toFixed(2) +
		"," +
		starmapCtrl.target.y.toFixed(2) +
		"," +
		starmapCtrl.target.z.toFixed(2) +
		"," +
		state.starmapPulses.length +
		"," +
		(starmapHovered ? starmapHovered.userData.nodeId : "") +
		"," +
		(starmapSelected ? starmapSelected.userData.nodeId : "") +
		"," +
		cw +
		"x" +
		ch;
	// ★ 节流：相机拖动（OrbitControls 阻尼）会让 camKey 每帧都变，
	//   若每帧重建 plan（3190 项）+ measureText，拖动时就是持续掉帧。
	//   标签是文字层，10fps 的刷新肉眼已看不出滞后（图形仍是 60fps）。
	var nowMs = Date.now();
	if (
		nowMs - starmapLabelLastDraw < SM_LABEL_MIN_INTERVAL ||
		camKey === starmapLabelLastCamKey
	)
		return;
	starmapLabelLastCamKey = camKey;
	starmapLabelLastDraw = nowMs;

	ctx.clearRect(0, 0, cw, ch);
	var plan = starmapMakeLabelPlan();
	// 同屏文本去重：3178 块里大量同文本块，不去重会叠成十几层同样的字。
	var drawnText = {};
	var dpr = starmapRen.getPixelRatio ? starmapRen.getPixelRatio() : 1;
	var v = new THREE.Vector3();
	var halfW = cw / 2,
		halfH = ch / 2;

	// 先画 dim，再画 hot —— hot 自然压在底纹上面。
	for (var i = 0; i < plan.length; i++) {
		var it = plan[i];
		if (it.kind === "dim" && drawnText[it.text]) continue;
		v.copy(it.mesh.position);
		v.project(starmapCam);
		// z>1 在相机后面；屏外则跳过。
		if (v.z > 1) continue;
		var sx = (v.x * 0.5 + 0.5) * cw,
			sy = (-v.y * 0.5 + 0.5) * ch;
		if (sx < -60 || sx > cw + 60 || sy < -20 || sy > ch + 20) continue;
		if (!drawnText[it.text]) {
			var font =
				(it.kind === "hot" ? "bold 12px " : "11px ") +
				'ui-sans-serif, system-ui, -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif';
			ctx.font = font;
			ctx.textAlign = "left";
			ctx.textBaseline = "middle";
			// ★ measureText 缓存：它是**同步布局**，每帧对上千个标签逐次调会把
			//   主线程卡住。同一串文字在同字体下的宽度只要字体未变就不会变。
			var mkey = font + "\u0000" + it.text;
			var tw = starmapTextWidth[mkey];
			if (tw === undefined) {
				tw = ctx.measureText(it.text).width;
				// 上限保护：极端情况下（不断变动的块文本）避免无界增长。
				// 超限时整表清空重填——比逐项 LRU 简单，而重填代价极低（一次性）。
				if (Object.keys(starmapTextWidth).length > SM_TEXTWIDTH_MAX) {
					starmapTextWidth = {};
				}
				starmapTextWidth[mkey] = tw;
			}
			// 名字画在节点**右侧**（左对齐是节点中心，留 7px 避让球体）。
			var tx = sx + 7,
				y = sy;
			if (tx + tw > cw) tx = sx - 7 - tw;
			if (it.kind === "hot") {
				// hot：深色底块 + 实色字，确保任何背景下都读得清。
				ctx.fillStyle = "rgba(8, 10, 20, 0.85)";
				ctx.fillRect(tx - 4, y - 9, tw + 8, 18);
				ctx.fillStyle = "#ffffff";
			} else {
				// dim：只给字描一层极淡的描边，保证淡而不消失。
				ctx.fillStyle = "rgba(200, 206, 216, 0.42)";
			}
			ctx.fillText(it.text, tx, y);
			drawnText[it.text] = 1;
		}
	}
}

function starmapAnimate() {
	if (!starmapAnimateRunning) return;
	starmapRaf = requestAnimationFrame(starmapAnimate);

	// ★ 不可见时不做任何工作（2026-10-08 修「非常卡」）。
	//
	// 为何必须这一步：本循环原先**从不停**——切到聊天/终端页签、甚至浏览器
	// 最小化到后台，它仍以 60fps 跑着每帧 3190 节点的全量遍历（渲染、标签、
	// 形变推进）。用户感知就是整页卡。
	//
	// 判据不用 document.hidden：星图可能在**未激活的页签**里（canvas 被搬到
	// 别的容器），此时页面可见但星图看不到——document.hidden 为 false，
	// 却白烧 CPU。故直接问「星图容器当前是否可见」。
	var cont = starmapActiveContainer();
	if (!cont || !cont.offsetParent) {
		// 不可见：只保留最小状态（停渲染）。下次可见时由 starmapWake 重新驱动。
		return;
	}

	if (starmapCtrl) starmapCtrl.update();
	// ★ 必须先判相机/控制器：starmapAnimate 可能被 starmapWake() 在相机尚未
	//   建好的时刻点火（例如 renderChatStarmap → initChatStarmap 前的路径）。
	//   下面 camKey 会直接读 starmapCam.position，无守卫时是 TypeError ——
	//   同一个缺陷在 GUI app.js 已实测抛出（报在 switchChatPanel 调用栈上）。
	if (!starmapRen || !starmapCam || !starmapCtrl) return;
	// 星空按**真实流逝时间**旋转（而非每帧固定 0.0001）。
	//
	// ★ 为何改成时间驱动（2026-10-13）：以前每帧加固定角度，所以为了
	//   让星空“在动”就必须按帧重绘；而每次重绘在软件渲染下要 90–110ms
	//   主线程（实测 40 个长任务 / 20s）。改成按 dt 推进后，**任何重绘
	//   频率下星空角度都是连续的**（不会因为降频而出现跳变），于是可以
	//   放心把空闲时的重绘频率大幅降低。
	var nowMs = performance.now();
	var dtSec = starmapLastFrameTs ? (nowMs - starmapLastFrameTs) / 1000 : 0;
	starmapLastFrameTs = nowMs;
	// 0.0001 弧度/帧 @60fps ⇒ 0.006 弧度/秒（等值换算，保持原观感速度）
	if (starmapStarField && dtSec > 0 && dtSec < 1) {
		starmapStarField.rotation.y += dtSec * 0.006;
	}
	// 推进活动脉冲 / 生长 / 呼吸（无活动时开销≈0）。
	if (starmapNodeMeshes.length) starmapTickActivity();
	// ★★ 2026-10-13：空闲帧跳过绘制。
	//
	// 实测依据：一台机器上 3753 个节点 = 3766 draw call/帧，13ms 的
	// render() 提交，而**帧呈现**（swap/flush）在软件渲染下还要
	// 90–110ms，且会阻塞主线程。静止画面按每秒两次重绘就是纯浪费。
	//
	// 判据取：相机姿态 + 活动集。任何交互（拖动/悬停/点击）或脉冲都会
	// 改变 camKey ⇒ 立即重绘，不存在“交互了但画面没跟上”。
	var camKey =
		starmapCam.position.x.toFixed(3) +
		"," +
		starmapCam.position.y.toFixed(3) +
		"," +
		starmapCam.position.z.toFixed(3) +
		"," +
		starmapCtrl.target.x.toFixed(3) +
		"," +
		starmapCtrl.target.y.toFixed(3) +
		"," +
		starmapCtrl.target.z.toFixed(3) +
		"," +
		state.starmapPulses.length +
		"," +
		(starmapMorphSet ? starmapMorphSet.size : -1) +
		"," +
		(starmapHovered ? starmapHovered.userData.nodeId : "") +
		"," +
		(starmapSelected ? starmapSelected.userData.nodeId : "") +
		"," +
		starmapLabelBaseVer;
	var changed = camKey !== starmapLastRenderKey;
	// 空闲心跳：保留一丝“画面还活着”的感知，但频率从 0.5s 降到 2s。
	// 由于星空已改成时间驱动，降低频率不会让星空跳动。
	if (!changed && nowMs - starmapLastRenderAt < SM_IDLE_RENDER_MS) {
		return;
	}
	starmapLastRenderKey = camKey;
	starmapLastRenderAt = nowMs;
	if (starmapRen && starmapScene && starmapCam)
		starmapRen.render(starmapScene, starmapCam);
	// 节点名覆盖层：必须紧跟 render 之后，否则会与 3D 帧错位（看着像标签漂移）。
	starmapDrawLabels();
}

// starmapWake 确保渲染循环在跑（幂等）。
//
// 为何需要：循环现在会在不可见时自行 return（见 starmapAnimate），
// 因此切回星图页签时必须重新点火——否则图冻在最后一帧。
function starmapWake() {
	if (starmapAnimateRunning) return;
	starmapAnimateRunning = true;
	starmapLabelLastCamKey = ""; // 强制标签重绘（可能已过去很久，相机也动过）
	if (starmapRaf) cancelAnimationFrame(starmapRaf);
	starmapAnimate();
}

// smStarmapFitDistance 估算能看到整个星图的相机距离。
//
// 为何不用固定值：节点数从 1294 变到 3178 后布局半径显著变大，
// 固定机位会只能看到中心一块，用户第一眼（首屏）就会觉得“图很乱/看不全”。
// 取 FOV 与当前节点规模算一个够用的距离即可，不需要精确。
function smStarmapFitDistance(w, h) {
	var n = starmapNodes.length || 1;
	// 与 buildChatStarmapGraph 同一套半径估算（每节点 18 单位²）。
	var r = Math.sqrt((n * 18) / Math.PI + 15 * 15);
	if (r > 260) r = 260;
	var fov = (60 * Math.PI) / 180;
	var aspect = w && h ? w / h : 1.6;
	// 水平方向更窄时（aspect<1）以水平视场为准，否则会左右切边。
	var effFov = aspect < 1 ? 2 * Math.atan(Math.tan(fov / 2) * aspect) : fov;
	var d = (r * 1.6) / Math.tan(effFov / 2);
	return Math.max(40, Math.min(d, 1500));
}

function createNebula() {
	var nc = 500;
	var p = new Float32Array(nc * 3),
		cl = new Float32Array(nc * 3);
	for (var i = 0; i < nc; i++) {
		var i3 = i * 3;
		p[i3] = (Math.random() - 0.5) * 800;
		p[i3 + 1] = (Math.random() - 0.5) * 800;
		p[i3 + 2] = (Math.random() - 0.5) * 800;
		var ch = Math.random();
		if (ch < 0.33) {
			cl[i3] = 0.5 + Math.random() * 0.3;
			cl[i3 + 1] = 0.2 + Math.random() * 0.2;
			cl[i3 + 2] = 0.7 + Math.random() * 0.3;
		} else if (ch < 0.66) {
			cl[i3] = 0.2 + Math.random() * 0.2;
			cl[i3 + 1] = 0.3 + Math.random() * 0.3;
			cl[i3 + 2] = 0.8 + Math.random() * 0.2;
		} else {
			cl[i3] = 0.7 + Math.random() * 0.3;
			cl[i3 + 1] = 0.2 + Math.random() * 0.2;
			cl[i3 + 2] = 0.5 + Math.random() * 0.3;
		}
	}
	var g = new THREE.BufferGeometry();
	g.setAttribute("position", new THREE.BufferAttribute(p, 3));
	g.setAttribute("color", new THREE.BufferAttribute(cl, 3));
	var m = new THREE.PointsMaterial({
		size: 8,
		vertexColors: true,
		transparent: true,
		opacity: 0.15,
		sizeAttenuation: true,
		blending: THREE.AdditiveBlending,
	});
	var np = new THREE.Points(g, m);
	starmapScene.add(np);
}

// ===== Settings =====
function pluginDisplayName(p) {
	if (p === "core") return __("核心", "Core");
	var name = p.replace("plugin.", "");
	var meta = state.pluginMeta && state.pluginMeta[name];
	if (meta)
		return state.lang === "en" ? meta.name_en || name : meta.name_zh || name;
	return name;
}

// toggleSecretVisibility 在「遮蔽 / 明文」间切换敏感输入框。
// 内联 onclick 需要全局可见，故挂到 window。
function toggleSecretVisibility(inpId) {
	var el = document.getElementById(inpId);
	if (!el) return;
	var showing = el.type === "text";
	el.type = showing ? "password" : "text";
	var btn = el.parentElement ? el.parentElement.querySelector("button") : null;
	if (btn) btn.textContent = showing ? __("显示", "Show") : __("隐藏", "Hide");
}
if (typeof window !== "undefined")
	window.toggleSecretVisibility = toggleSecretVisibility;

function renderSettingsSidebar() {
	var el = document.querySelector(".settings-tabs");
	if (!el) return;
	el.innerHTML = "";
	// 「外观」不是插件设置，但它是 webui 自己的配置，放在设置页最前面。
	var ap = document.createElement("span");
	ap.textContent = __("外观", "Appearance");
	if (state.selectedSection === "appearance") ap.className = "active";
	ap.onclick = () => {
		state.selectedSection = "appearance";
		renderOneSettings();
	};
	el.appendChild(ap);
	state.settingsPlugins.forEach((p) => {
		var a = document.createElement("span");
		a.textContent = pluginDisplayName(p);
		if (p === state.selectedSection) a.className = "active";
		a.onclick = () => {
			state.selectedSection = p;
			renderOneSettings();
		};
		el.appendChild(a);
	});
}

// 外观设置区：主题（浅/深）+ 配色 + 背景图。与侧栏那个 palette-pop 同一套
// setColor/applyBgImg/applyBgBlur，只是给它一个正式的、可发现的落点
// （原来只有侧栏底部一个调色盘图标，找不到）。
function appearanceHtml() {
	var theme = document.documentElement.getAttribute("data-theme") || "light";
	var color = localStorage.getItem("ha-color") || "mono";
	var dots = "";
	Object.keys(PALETTES).forEach((k) => {
		dots +=
			'<button class="ap-dot' +
			(k === color ? " on" : "") +
			'" data-c="' +
			k +
			'" title="' +
			k +
			'" style="background:' +
			PALETTES[k] +
			'" onclick="pickColor(\'' +
			k +
			"')\"></button>";
	});
	var img = localStorage.getItem("ha-bg-img") || "";
	var blur = localStorage.getItem("ha-bg-blur") || "0";
	return (
		'<div class="card"><h2>' +
		__("外观", "Appearance") +
		"</h2>" +
		'<div class="kv-row"><span class="key">' +
		__("主题", "Theme") +
		'</span><span class="val">' +
		'<button class="btn btn-sm' +
		(theme === "light" ? "" : " btn-ghost") +
		'" onclick="pickTheme(\'light\')">' +
		__("浅色", "Light") +
		"</button> " +
		'<button class="btn btn-sm' +
		(theme === "dark" ? "" : " btn-ghost") +
		'" onclick="pickTheme(\'dark\')">' +
		__("深色", "Dark") +
		"</button>" +
		"</span></div>" +
		'<div class="kv-row"><span class="key">' +
		__("配色", "Color") +
		'</span><span class="val"><span class="ap-dots">' +
		dots +
		"</span></span></div>" +
		"<label>" +
		__("背景图片 URL", "Background image URL") +
		"</label>" +
		'<input type="text" id="ap-bg-img" value="' +
		escHtml(img) +
		'" placeholder="https://...jpg / png">' +
		'<div style="display:flex;gap:8px;margin-top:8px">' +
		'<button class="btn btn-sm" onclick="applyBgImg(document.getElementById(\'ap-bg-img\').value)">' +
		__("应用", "Apply") +
		"</button>" +
		"<button class=\"btn btn-ghost btn-sm\" onclick=\"applyBgImg('');document.getElementById('ap-bg-img').value=''\">" +
		__("清除", "Clear") +
		"</button>" +
		"</div>" +
		'<label style="margin-top:12px">' +
		__("背景模糊", "Background blur") +
		'：<span id="ap-blur-val">' +
		blur +
		"px</span></label>" +
		'<input type="range" id="ap-blur-range" min="0" max="30" value="' +
		blur +
		"\" oninput=\"applyBgBlur(this.value);var v=document.getElementById('ap-blur-val');if(v)v.textContent=this.value+'px'\">" +
		'<p style="color:var(--text-muted);font-size:12px;margin-top:10px">' +
		__(
			"配色只影响本站点界面的强调色，不改动 Agent 的人格或数据。",
			"Colors only affect this dashboard's accent; agent behavior and data are unchanged.",
		) +
		"</p></div>"
	);
}
function pickTheme(name) {
	saveTheme(name);
	renderOneSettings();
}
function pickColor(name) {
	setColor(name);
	renderOneSettings();
}

function renderOneSettings() {
	if (state.selectedSection === "appearance") {
		document.getElementById("tab-settings").innerHTML =
			'<div class="settings-layout"><div class="settings-tabs"></div><div class="settings-content">' +
			appearanceHtml() +
			"</div></div>";
		renderSettingsSidebar();
		return;
	}
	var prefix = state.selectedSection + ".";
	var allKeys = Object.keys(state.settings || {});
	var filtered = allKeys.filter(
		(k) => k === prefix.slice(0, -1) || k.startsWith(prefix),
	);
	filtered.sort();
	var hideTopLlms = [
		"core.llm.base_url",
		"core.llm.model",
		"core.llm.api_key",
		"core.llm.adapter",
		"core.llm.adapter_path",
		"core.llm.thinking_enabled",
	];
	var sourceKeys = filtered.filter((k) => k.startsWith("core.llm.sources."));
	var sourceMap = {};
	sourceKeys.forEach((k) => {
		var parts = k.split(".");
		var srcName = parts[3];
		if (!sourceMap[srcName]) sourceMap[srcName] = {};
		sourceMap[srcName][k] = true;
	});
	var mcpServerKeys = filtered.filter(
		(k) => k.startsWith("plugin.mcp.servers.") && k.split(".").length >= 5,
	);
	var mcpServerMap = {};
	mcpServerKeys.forEach((k) => {
		var parts = k.split(".");
		var srvName = parts[3];
		if (!mcpServerMap[srvName]) mcpServerMap[srvName] = {};
		mcpServerMap[srvName][k] = true;
	});
	var regularKeys = filtered.filter(
		(k) =>
			!k.startsWith("core.llm.sources.") &&
			hideTopLlms.indexOf(k) === -1 &&
			!k.startsWith("plugin.mcp.servers.") &&
			k !== "plugin.mcp.servers",
	);
	var html =
		'<div class="settings-layout"><div class="settings-tabs"></div><div class="settings-content">';
	if (
		regularKeys.length === 0 &&
		Object.keys(sourceMap).length === 0 &&
		Object.keys(mcpServerMap).length === 0 &&
		state.selectedSection !== "plugin.mcp"
	) {
		html +=
			'<div class="card"><h2>' +
			escHtml(state.selectedSection) +
			'</h2><p style="color:var(--text-muted)">' +
			__("暂无设置项", "No settings") +
			"</p></div>";
	} else {
		regularKeys.forEach((k) => {
			var v = state.settings[k];
			var sv = typeof v === "object" ? JSON.stringify(v) : String(v);
			var m = state.meta?.[k];
			var shortName = k.split(".").pop().replace(/_/g, " ");
			var label = m?.display_name || shortName;
			var desc = m?.description || "";
			var typ = m?.type || "string";
			var ph = m?.placeholder || "";
			var opts = m?.options || [];
			var inpId = "inp-" + k.replace(/\./g, "_");
			var inp = "";
			if (typ === "bool") {
				var chk = sv === "true" ? "checked" : "";
				inp =
					'<label style="display:flex;align-items:center;gap:8px;cursor:pointer"><input type="checkbox" id="' +
					inpId +
					'" ' +
					chk +
					" onchange=\"markDirty('" +
					k +
					'\')" style="width:auto;margin:0"> ' +
					label +
					"</label>";
			} else if (typ === "select") {
				var selOpts = "";
				opts.forEach((o) => {
					selOpts +=
						'<option value="' +
						o +
						'"' +
						(sv === o ? " selected" : "") +
						">" +
						o +
						"</option>";
				});
				inp =
					"<label>" +
					label +
					'</label><select id="' +
					inpId +
					'" onchange="markDirty(\'' +
					k +
					"')\">" +
					selOpts +
					"</select>";
			} else if (typ === "password" || m?.secret) {
				// 敏感项：默认遮蔽，点「显示」才明文。
				//
				// 两种声明都走这里：Type=="password"（旧写法）与 Secret:true
				// （新写法）。值本身是明文（内核透明解密），遮蔽只防肩窥/截屏，
				// 不靠前端做保密 —— 真正的保护在落盘加密那一层。
				inp =
					"<label>" +
					label +
					'</label><div style="display:flex;gap:6px;align-items:center">' +
					'<input type="password" id="' +
					inpId +
					'" value="' +
					escHtml(sv) +
					'" placeholder="' +
					escHtml(ph) +
					'" onchange="markDirty(\'' +
					k +
					'\')" style="flex:1" autocomplete="new-password">' +
					'<button type="button" class="btn-sm" style="flex:0 0 auto" onclick="toggleSecretVisibility(\'' +
					inpId +
					"')\">" +
					__("显示", "Show") +
					"</button></div>";
			} else if (typ === "text") {
				inp =
					"<label>" +
					label +
					'</label><textarea id="' +
					inpId +
					'" placeholder="' +
					escHtml(ph) +
					'" onchange="markDirty(\'' +
					k +
					"')\">" +
					escHtml(sv) +
					"</textarea>";
			} else {
				inp =
					"<label>" +
					label +
					'</label><input type="text" id="' +
					inpId +
					'" value="' +
					escHtml(sv) +
					'" placeholder="' +
					escHtml(ph) +
					'" onchange="markDirty(\'' +
					k +
					"')\">";
			}
			var extra = "";
			if (m?.extra) {
				m.extra.forEach((f) => {
					var fk = (k ? k + "." : "") + f.key;
					var fv = state.settings?.[fk];
					var fph = f.placeholder || __("输入", "Enter ") + f.label;
					extra +=
						'<div class="form-row" style="margin-left:16px;margin-top:4px"><label>' +
						f.label +
						"</label>";
					if (f.type === "select") {
						var fopts = "";
						if (f.options)
							f.options.forEach((o) => {
								fopts +=
									'<option value="' +
									o +
									'"' +
									(fv === o ? "selected" : "") +
									">" +
									o +
									"</option>";
							});
						extra +=
							"<select onchange=\"saveSetting('" +
							fk +
							"',this.value)\">" +
							fopts +
							"</select>";
					} else {
						extra +=
							'<input type="text" value="' +
							escHtml(fv || "") +
							'" placeholder="' +
							escHtml(fph) +
							'" onchange="markDirty(\'' +
							fk +
							'\')" style="margin-bottom:0">';
					}
					extra += "</div>";
				});
			}
			var descHtml = desc
				? '<p style="font-size:11px;color:var(--text-muted);margin:-6px 0 10px">' +
					escHtml(desc) +
					"</p>"
				: "";
			html +=
				'<div class="card"><div class="settings-key">' +
				escHtml(k) +
				"</div>" +
				inp +
				descHtml +
				extra +
				'<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border);margin-top:4px" onclick="saveSetting(\'' +
				k +
				"')\">" +
				__("保存", "Save") +
				"</button></div>";
		});
		// LLM Sources
		Object.keys(sourceMap)
			.sort()
			.forEach((src) => {
				var baseKey = "core.llm.sources." + src;
				var srcData =
					state.settings?.[baseKey + ".adapter"] ||
					state.settings?.[baseKey + ".base_url"] ||
					"";
				var fields = [
					{
						key: "adapter",
						label: __("适配器", "Adapter"),
						type: "text",
					},
					{ key: "base_url", label: "Base URL", type: "text" },
					{ key: "model", label: __("模型", "Model"), type: "text" },
					{ key: "api_key", label: "API Key", type: "text" },
					{
						key: "thinking_enabled",
						label: __("思考模式", "Thinking Mode"),
						type: "select",
						options: ["true", "false"],
					},
					{
						key: "adapter_path",
						label: __("适配器路径", "Adapter Path"),
						type: "text",
					},
				];
				var headerLabel = mL10n(src, "LLM Source: " + src);
				html += '<div class="card"><h2>' + escHtml(headerLabel) + "</h2>";
				fields.forEach((f) => {
					var fk = baseKey + "." + f.key;
					var fv = state.settings?.[fk] || "";
					var flabel = f.label;
					var fieldId = "inp-" + fk.replace(/\./g, "_");
					if (f.type === "select") {
						var fopts = "";
						f.options.forEach((o) => {
							fopts +=
								'<option value="' +
								o +
								'"' +
								(fv === o ? "selected" : "") +
								">" +
								o +
								"</option>";
						});
						html +=
							"<label>" +
							flabel +
							'</label><select id="' +
							fieldId +
							'" onchange="markDirty(\'' +
							fk +
							'\')" style="margin-bottom:4px">' +
							fopts +
							"</select>";
					} else {
						html +=
							"<label>" +
							flabel +
							'</label><input type="text" id="' +
							fieldId +
							'" value="' +
							escHtml(fv) +
							'" placeholder="' +
							(f.key === "api_key"
								? __("输入 API Key", "Enter API Key")
								: __("输入", "Enter ") + flabel) +
							'" onchange="markDirty(\'' +
							fk +
							'\')" style="margin-bottom:4px">';
					}
				});
				html +=
					'<div style="display:flex;gap:8px;margin-top:8px">' +
					'<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' +
					baseKey +
					".adapter');saveSetting('" +
					baseKey +
					".base_url');saveSetting('" +
					baseKey +
					".model');saveSetting('" +
					baseKey +
					".api_key');var inp=document.getElementById('" +
					("inp-" + baseKey + ".thinking_enabled").replace(/\./g, "_") +
					"');if(inp)saveSetting('" +
					baseKey +
					".thinking_enabled');toast('" +
					__("源", "Source") +
					" \\'" +
					src +
					"\\' " +
					__("已保存", "saved") +
					"')\">" +
					__("保存", "Save") +
					"</button>" +
					'<button class="btn btn-sm btn-danger" onclick="deleteSource(\'' +
					src +
					"')\">" +
					__("删除", "Delete") +
					"</button></div></div>";
			});
		if (
			Object.keys(sourceMap).length > 0 ||
			state.selectedSection === "core.llm"
		) {
			html +=
				'<button class="btn btn-ghost btn-sm" onclick="showAddSourceDialog()" style="margin-bottom:16px">+ ' +
				__("添加 LLM 源", "Add LLM Source") +
				"</button>";
		}
		// MCP Servers
		if (
			state.selectedSection === "plugin.mcp" ||
			Object.keys(mcpServerMap).length > 0
		) {
			html +=
				'<div class="card"><h2>' +
				__("MCP 服务器", "MCP Servers") +
				'</h2><p style="font-size:11px;color:var(--text-muted);margin-bottom:8px">' +
				__(
					"配置 Model Context Protocol 服务端连接",
					"Configure Model Context Protocol server connections",
				) +
				"</p></div>";
			Object.keys(mcpServerMap)
				.sort()
				.forEach((srv) => {
					var baseKey = "plugin.mcp.servers." + srv;
					var fields = [
						{
							key: "command",
							label: __("启动命令", "Command"),
							type: "text",
						},
						{ key: "url", label: "SSE URL", type: "text" },
						{
							key: "args",
							label: __("参数(JSON数组)", "Args (JSON array)"),
							type: "text",
						},
						{
							key: "env",
							label: __("环境变量(JSON数组)", "Env (JSON array)"),
							type: "text",
						},
					];
					html += '<div class="card"><h2>' + escHtml(srv) + "</h2>";
					fields.forEach((f) => {
						var fk = baseKey + "." + f.key;
						var fv = state.settings?.[fk] || "";
						var fieldId = "inp-" + fk.replace(/\./g, "_");
						html +=
							"<label>" +
							f.label +
							'</label><input type="text" id="' +
							fieldId +
							'" value="' +
							escHtml(fv) +
							'" placeholder="' +
							__("输入", "Enter ") +
							f.label +
							'" onchange="markDirty(\'' +
							fk +
							'\')" style="margin-bottom:4px">';
					});
					html +=
						'<div style="display:flex;gap:8px;margin-top:8px">' +
						'<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' +
						baseKey +
						".command');saveSetting('" +
						baseKey +
						".url');saveSetting('" +
						baseKey +
						".args');saveSetting('" +
						baseKey +
						".env');toast('MCP \\'" +
						srv +
						"\\' " +
						__("已保存", "saved") +
						"')\">" +
						__("保存", "Save") +
						"</button>" +
						'<button class="btn btn-sm btn-danger" onclick="deleteMCPServer(\'' +
						srv +
						"')\">" +
						__("删除", "Delete") +
						"</button></div></div>";
				});
			html +=
				'<button class="btn btn-ghost btn-sm" onclick="addMCPSource()" style="margin-bottom:16px">+ ' +
				__("添加 MCP 服务器", "Add MCP Server") +
				"</button>";
		}
	}
	html += "</div></div>";
	document.getElementById("tab-settings").innerHTML = html;
	renderSettingsSidebar();
}

function markDirty(k) {
	var inp = document.getElementById("inp-" + k.replace(/\./g, "_"));
	if (inp) inp.style.borderColor = "var(--save-btn-border)";
}

async function saveSetting(k) {
	var inp = document.getElementById("inp-" + k.replace(/\./g, "_"));
	if (!inp) return;
	// ★ 掩码占位符**不是值**，绝不能回传。
	//
	// 真实故障（2026-10-16）：列表接口为防泄漏把敏感值换成 `••••••••`，
	// 而设置页「保存」按钮会**一并回传**该组的全部字段（含刚被掩码的 api_key）
	// ⇒ 掩码被当真值存进内核 ⇒ 下游（commondev 的 pi 源导入器）把掩码
	// 写进 auth.json ⇒ 模型调用 401 invalid api key。
	//
	// 两层防护：这里（前端不回传）与后端（PUT 拒绝占位符）。
	// 前端这层必须独立成立：后端拒绝会让整组保存失败，用户以为"保存坏了"。
	if (isSecretPlaceholderValue(inp.value)) {
		// 未改动密钥时静默跳过（不该弹错——用户只是想改别的字段）
		return;
	}
	var val;
	var m = state.meta?.[k];
	if (m?.type === "bool") {
		val = inp.checked ? "true" : "false";
	} else if (m?.type === "select") {
		val = inp.value;
	} else {
		var raw = inp.value;
		try {
			val = JSON.parse(raw);
		} catch (e) {
			val = raw;
		}
	}
	try {
		var r = await api("/settings", {
			method: "PUT",
			body: JSON.stringify({ key: k, value: val }),
		});
		if (r.status === "ok") {
			inp.style.borderColor = "";
			state.settings[k] = val;
			toast(__("已保存: ", "Saved: ") + k);
		} else {
			toast(__("保存失败: ", "Save failed: ") + (r.error || "unknown"), true);
		}
	} catch (e) {
		toast(__("保存失败: ", "Save failed: ") + e.message, true);
	}
}

// isSecretPlaceholderValue 判定输入框里的是否是掩码占位符（而非真实值）。
//
// 用**全等**而不是 includes：真 key 里恰好含这两个字符的概率极低，
// 但反过来说，只要不是"整个值就是掩码"，就一律放行 —— 宁可放行也不误杀真值。
function isSecretPlaceholderValue(v) {
	return (
		typeof v === "string" &&
		v.trim() === "\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022"
	);
}

function renderConfigDisabled() {
	document.getElementById("tab-settings").innerHTML =
		'<div class="card"><h2>' +
		__("设置", "Settings") +
		'</h2><p style="color:var(--text-muted)">' +
		__("设置面板已加载", "Settings panel loaded") +
		"</p></div>";
	renderOneSettings();
}

function mL10n(key, fallback) {
	var meta = state.meta?.[key];
	if (meta?.display_name) return meta.display_name;
	return fallback || key;
}

function showAddSourceDialog() {
	var name = prompt(
		__(
			"输入新 LLM 源名称（如 openai、anthropic）:",
			"Enter new LLM source name (e.g. openai, anthropic):",
		),
	);
	if (!name || !name.trim()) return;
	name = name
		.trim()
		.toLowerCase()
		.replace(/[^a-z0-9_]/g, "_");
	if (!name) {
		toast(__("名称无效", "Invalid name"), true);
		return;
	}
	var keys = [
		"base_url",
		"model",
		"api_key",
		"adapter",
		"adapter_path",
		"thinking_enabled",
	];
	var values = {
		base_url: "https://api." + name + ".com",
		model: "",
		api_key: "",
		adapter: name,
		adapter_path: "",
		thinking_enabled: "false",
	};
	var promises = keys.map((f) =>
		api("/settings", {
			method: "PUT",
			body: JSON.stringify({
				key: "core.llm.sources." + name + "." + f,
				value: values[f],
			}),
		}),
	);
	Promise.all(promises)
		.then(() => {
			toast(
				__("源", "Source") +
					' "' +
					name +
					'" ' +
					__("已创建，请配置各项参数", "created, please configure parameters"),
			);
			renderAll(true);
		})
		.catch((e) => {
			toast(__("创建失败: ", "Create failed: ") + e.message, true);
		});
}

async function deleteSource(name) {
	if (
		!confirm(
			__("确认删除源", "Are you sure to delete source") + ' "' + name + '"?',
		)
	)
		return;
	var base = "core.llm.sources." + name;
	var fields = [
		"adapter",
		"base_url",
		"model",
		"api_key",
		"thinking_enabled",
		"adapter_path",
	];
	try {
		for (var f of fields) {
			await api("/settings", {
				method: "PUT",
				body: JSON.stringify({ key: base + "." + f, value: null }),
			});
		}
		toast(__("源", "Source") + ' "' + name + '" ' + __("已删除", "deleted"));
		renderAll(true);
	} catch (e) {
		toast(__("删除失败: ", "Delete failed: ") + e.message, true);
	}
}

function addMCPSource() {
	var name = prompt(__("输入新 MCP 服务器名称:", "Enter new MCP server name:"));
	if (!name || !name.trim()) return;
	name = name
		.trim()
		.toLowerCase()
		.replace(/[^a-z0-9_]/g, "_");
	if (!name) {
		toast(__("名称无效", "Invalid name"), true);
		return;
	}
	var fields = ["command", "url", "args", "env"];
	var values = { command: "", url: "", args: "[]", env: "[]" };
	var promises = fields.map((f) =>
		api("/settings", {
			method: "PUT",
			body: JSON.stringify({
				key: "plugin.mcp.servers." + name + "." + f,
				value: values[f],
			}),
		}),
	);
	Promise.all(promises)
		.then(() => {
			toast(
				"MCP " +
					__("服务器", "server") +
					' "' +
					name +
					'" ' +
					__("已创建", "created"),
			);
			renderAll(true);
		})
		.catch((e) => {
			toast(__("创建失败: ", "Create failed: ") + e.message, true);
		});
}

async function deleteMCPServer(name) {
	if (
		!confirm(
			__("确认删除 MCP 服务器", "Are you sure to delete MCP server") +
				' "' +
				name +
				'"?',
		)
	)
		return;
	var fields = ["command", "url", "args", "env"];
	try {
		for (var f of fields) {
			await api("/settings", {
				method: "PUT",
				body: JSON.stringify({
					key: "plugin.mcp.servers." + name + "." + f,
					value: null,
				}),
			});
		}
		toast(
			"MCP " +
				__("服务器", "server") +
				' "' +
				name +
				'" ' +
				__("已删除", "deleted"),
		);
		renderAll(true);
	} catch (e) {
		toast(__("删除失败: ", "Delete failed: ") + e.message, true);
	}
}

// ===== Adapters =====
async function renderAdapters() {
	var html = "";
	try {
		var r = await api("/adapters");
		var adapters = r.adapters || [];
		window._adapters = adapters;
		html +=
			'<div class="card"><h2>' +
			__("已加载的适配器", "Loaded Adapters") +
			" (" +
			adapters.length +
			")</h2>";
		if (adapters.length === 0) {
			html +=
				'<p style="color:var(--text-muted)">' +
				__("暂无适配器", "No adapters") +
				"</p>";
		} else {
			html +=
				"<table><tr><th>" +
				__("名称", "Name") +
				"</th><th>" +
				__("版本", "Version") +
				"</th><th>" +
				__("操作", "Actions") +
				"</th></tr>";
			adapters.forEach((a) => {
				html +=
					"<tr><td>" +
					escHtml(a.name) +
					"</td><td>" +
					escHtml(a.version || "-") +
					"</td>" +
					'<td><button class="btn btn-danger btn-sm" onclick="deleteAdapter(\'' +
					escHtml(a.name) +
					"')\">" +
					__("删除", "Delete") +
					"</button></td></tr>";
			});
			html += "</table>";
		}
		html += "</div>";
		html +=
			'<div class="card"><h2>' +
			__("上传新适配器", "Upload New Adapter") +
			"</h2>" +
			"<label>" +
			__("适配器名称（不带 .lua）", "Adapter name (without .lua)") +
			"</label>" +
			'<input id="adapter-name" placeholder="' +
			__("如 openai", "e.g. openai") +
			'">' +
			"<label>" +
			__("Lua 脚本代码", "Lua Script Code") +
			"</label>" +
			'<textarea id="adapter-code" rows="12" placeholder="-- ' +
			__("返回一个适配器表", "return an adapter table") +
			'\nreturn {\n  name = &quot;openai&quot;,\n  version = &quot;1.0&quot;,\n  transform_request = function(raw) ... end,\n  transform_response = function(raw) ... end,\n}"></textarea>' +
			'<button class="btn btn-primary" onclick="uploadAdapter()">' +
			__("上传", "Upload") +
			"</button></div>";
	} catch (e) {
		html +=
			'<div class="card"><p style="color:var(--text-muted)">' +
			__("加载适配器失败: ", "Failed to load adapters: ") +
			escHtml(e.message) +
			"</p></div>";
	}
	document.getElementById("tab-adapters").innerHTML = html;
}

async function uploadAdapter() {
	var name = document.getElementById("adapter-name")?.value;
	var code = document.getElementById("adapter-code")?.value;
	if (!name || !code) {
		toast(__("名称和代码不能为空", "Name and code cannot be empty"), true);
		return;
	}
	try {
		var r = await api("/adapters", {
			method: "POST",
			body: JSON.stringify({ name: name, code: code }),
		});
		if (r.status === "loaded") {
			toast(
				__("适配器", "Adapter") + ' "' + name + '" ' + __("已加载", "loaded"),
			);
			renderAdapters();
		} else {
			toast(__("上传失败: ", "Upload failed: ") + (r.error || "unknown"), true);
		}
	} catch (e) {
		toast(__("上传失败: ", "Upload failed: ") + e.message, true);
	}
}

async function deleteAdapter(name) {
	if (
		!confirm(
			__("确定删除适配器", "Are you sure to delete adapter") +
				' "' +
				name +
				'"？',
		)
	)
		return;
	try {
		var r = await api("/adapters/" + encodeURIComponent(name), {
			method: "DELETE",
		});
		if (r.status === "deleted") {
			toast(
				__("适配器", "Adapter") + ' "' + name + '" ' + __("已删除", "deleted"),
			);
			renderAdapters();
		} else {
			toast(__("删除失败", "Delete failed"), true);
		}
	} catch (e) {
		toast(__("删除失败: ", "Delete failed: ") + e.message, true);
	}
}

// ===== Init =====
async function logout() {
	try {
		await fetch("/api/v1/logout", {
			method: "POST",
			credentials: "include",
		});
	} catch (e) {}
	location.href = "/login";
}

renderConfigDisabled();
// 先加载历史再连 SSE：避免 SSE 事件先到与历史加载顺序不确定导致消息重复/丢失
(async () => {
	await loadChatHistory();
	renderAll();
	connectSSE();
	startUptimeTicker();
	startRuntimeTicker();
	startChatTicker();
	maybeShowPersonaWizard();
})();
setInterval(renderAll, 15000);
// 消息同步轮询兜底：每30秒增量同步 chatHistory，补偿 SSE 断连窗口期
// 丢失的事件（尤其是非 WebUI 触发的跨渠道消息，如 CLI/QQ/设备桥输出）。
// syncChatFromHistory 仅追加新消息 DOM 节点，不重建已有消息，无闪烁。
setInterval(() => {
	syncChatFromHistory().catch(() => {});
}, 30000);

// ===== 内核控制：重启 / 关机 =====
//
// 两个动作都会让内核进程消失，区别是：
//   · 重启 → guard 用当前二进制重拉，断线数秒后页面可自动重连
//   · 关机 → systemd 不再拉起，必须人工 systemctl start
//
// ★ 关机必须**二次确认且说清后果**：它不像重启那样能自己回来，
//   误点一次就得 ssh 上去恢复。用原生 confirm（与其它危险操作一致）。
async function kernelRestart() {
	if (
		!confirm(
			"重启内核？\n\n服务会断线约数秒，之后自动恢复。\n正在进行中的对话可能中断。",
		)
	)
		return;
	await doKernelControl("restart", "内核正在重启，页面将在数秒后自动重连…");
	// 重启：轮询等它回来
	waitForKernelBack();
}

async function kernelShutdown() {
	if (
		!confirm(
			"关闭内核？\n\n⚠ 关机后**不会自动启动**，必须到机器上执行：\n\n    systemctl start homeagent\n\n确认要关闭吗？",
		)
	) {
		return;
	}
	await doKernelControl(
		"shutdown",
		"内核正在关闭。需要人工执行 systemctl start 才能再启动。",
	);
	// 关机：不再轮询（它不会自己回来），把页面标记为已停
	document.body.innerHTML =
		'<div style="display:flex;height:100vh;align-items:center;justify-content:center;' +
		'font:16px/1.6 system-ui;color:#888;text-align:center;padding:2em">' +
		"<div><h2 style='margin:0 0 .5em'>内核已关闭</h2>" +
		"<p>需要人工启动：<code>systemctl start homeagent</code></p></div></div>";
}

async function doKernelControl(action, okMsg) {
	var reason = "WebUI 操作";
	try {
		var r = await api("/kernel/" + action, {
			method: "POST",
			body: JSON.stringify({ reason: reason }),
		});
		if (r && r.error) {
			toast(r.error, true);
			return false;
		}
		toast(okMsg);
		return true;
	} catch (e) {
		// ★ 这里**可能是正常的**：内核收到请求后很快就退出了，
		// fetch 因此拿到 connection reset。不能一律当失败报错。
		if (action === "restart") {
			toast("内核已收到重启请求（连接中断属正常）");
			return true;
		}
		toast("请求已发出（连接中断属正常）");
		return true;
	}
}

// waitForKernelBack 在重启后轮询 /status 直到内核回来，然后刷新页面。
//
// 最长等 60s：实测冷启动 1.5-2s，但机器忙时可能更久；
// 超时就明确告诉用户"没回来，请查看服务状态"，而不是无限转圈。
async function waitForKernelBack() {
	var deadline = Date.now() + 60000;
	var showed = false;
	while (Date.now() < deadline) {
		await new Promise((r) => setTimeout(r, 1500));
		try {
			var r = await fetch("/api/v1/status", { credentials: "include" });
			if (r.ok) {
				toast("内核已恢复，正在刷新…");
				await new Promise((r2) => setTimeout(r2, 600));
				location.reload();
				return;
			}
		} catch (_) {
			// 还在重启中，继续等
		}
		if (!showed) {
			showed = true;
		}
	}
	toast("内核 60 秒内未恢复。请检查：systemctl status homeagent", true);
}
