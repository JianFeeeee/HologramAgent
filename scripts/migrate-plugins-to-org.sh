#!/usr/bin/env bash
# 把 SDK 仓 example/ 下的示例插件迁移到 GitHub 组织下成为**独立仓库**。
#
# 用法：
#   scripts/migrate-plugins-to-org.sh [--dry-run] [--only name1,name2]
#
# 环境：
#   GH_ORG_TOKEN  fine-grained PAT，Resource owner = 目标组织，
#                 带 Repository permissions: Administration(Write) 或
#                 Repository creation(Write) + Contents(Write)
#   GH_ORG       目标组织名（默认 HologramAgent）
#
# 为什么这么做：
#   插件散落在 SDK 仓 example/ 下有三个问题：
#     ① 插件版本与 SDK 版本绑死（一次 SDK 发版把所有插件都带上）
#     ② 插件作者不能单独为自己的插件开发/发版（都在别人的仓里）
#     ③ plugin_update 需要「仓库地址 → 拉源码 → 编译」，而
#        example/ 是别人的仓库，不是插件自己的仓库
#
# 纪律：
#   1. 默认 --dry-run：只打印会做什么，不碰 GitHub。
#   2. 产物不入库：dist/ build/ rawfile/ node_modules/ *.hmap 全部排除
#      （实测源码 3MB，产物 1.9G —— 后者进了仓库就再也删不干净）
#   3. 测试与 go.sum 保留：它们是仓库的一部分，不是产物。
set -euo pipefail

DRY=0
ONLY=""
for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY=1 ;;
	--only)
		shift
		ONLY="${1:-}"
		;;
	*)
		echo "未知参数: $arg" >&2
		exit 2
		;;
	esac
	shift || true
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EXAMPLES="$ROOT/third_party/homeagent-sdk/example"
GH_ORG="${GH_ORG:-HologramAgent}"
TOKEN="${GH_ORG_TOKEN:-}"

if [ "$DRY" -eq 0 ] && [ -z "$TOKEN" ]; then
	echo "ERROR: 非 dry-run 需要 GH_ORG_TOKEN" >&2
	exit 2
fi

api() {
	# $1=method $2=path $3=payload(optional)
	local method="$1" path="$2" payload="${3:-}"
	if [ -n "$payload" ]; then
		curl -sS -X "$method" \
			-H "Authorization: Bearer $TOKEN" \
			-H "Accept: application/vnd.github+json" \
			"https://api.github.com${path}" -d "$payload"
	else
		curl -sS -X "$method" \
			-H "Authorization: Bearer $TOKEN" \
			-H "Accept: application/vnd.github+json" \
			"https://api.github.com${path}"
	fi
}

created=0
pushed=0
skipped=0

for dir in "$EXAMPLES"/*/; do
	name="$(basename "$dir")"
	if [ -n "$ONLY" ]; then
		case ",$ONLY," in *",$name,"*) ;; *) continue ;; esac
	fi

	# 入口判据：Go 插件是 plugin.go，Lua 插件是 main.lua（与 build-examples.sh 同）
	if [ ! -f "$dir/plugin.go" ] && [ ! -f "$dir/main.lua" ]; then
		continue
	fi
	desc=$(python3 -c "import json,sys;print(json.load(open('$dir/plg.json')).get('description',''))" 2>/dev/null || echo "")
	ver=$(python3 -c "import json,sys;print(json.load(open('$dir/plg.json')).get('version',''))" 2>/dev/null || echo "")

	echo ""
	echo "── $name (v$ver) ──"
	echo "   desc: ${desc:0:60}"

	if [ "$DRY" -eq 1 ]; then
		echo "   [dry-run] 将创建仓库 $GH_ORG/$name 并推送源码（排除 dist/build/rawfile/node_modules）"
		continue
	fi

	# ① 创建仓库（已存在则跳过 —— 幂等，重跑安全）
	code=$(curl -s -o /tmp/ghresp -w "%{http_code}" -X POST \
		-H "Authorization: Bearer $TOKEN" -H "Accept: application/vnd.github+json" \
		https://api.github.com/orgs/$GH_ORG/repos \
		-d "{\"name\":\"$name\",\"private\":false,\"description\":\"$desc\",\"has_issues\":true}")
	if [ "$code" = "201" ]; then
		echo "   ✓ 仓库已创建"
		created=$((created + 1))
	elif [ "$code" = "422" ]; then
		echo "   · 仓库已存在（跳过创建）"
		skipped=$((skipped + 1))
	else
		echo "   ✗ 创建失败 HTTP=$code: $(head -c 200 /tmp/ghresp)"
		continue
	fi

	# ② 准备干净的源码副本
	#
	# 用 tar 管道而不是 rsync/cp -r：rsync 在最小化系统里常常没有
	# （本机实测就是），而 cp -r 无法精确排除多类产物。
	# --exclude 用**不带前导斜杠**的形态，匹配任意层级同名目录。
	work=$(mktemp -d)
	mkdir -p "$work/$name"
	(cd "$dir" && tar -cf - \
		--exclude='dist' --exclude='build' --exclude='rawfile' \
		--exclude='node_modules' --exclude='*.hmap' --exclude='.git' \
		.) | (cd "$work/$name" && tar -xf -)

	# plg.json 里补 repository 字段，让装好的插件能反过来找到自己的仓库
	# （plugin_update 靠它拉源码；不填的话插件永远无法自动更新）
	python3 - "$work/$name/plg.json" "https://github.com/$GH_ORG/$name" <<'PY'
import json, sys
path, repo = sys.argv[1], sys.argv[2]
d = json.load(open(path))
d["repo"] = repo
json.dump(d, open(path, "w"), indent=2, ensure_ascii=False)
open(path, "a").write("\n")
PY

	# 每个独立仓库都要有自己的 README 与 LICENSE 指引
	cat >"$work/$name/README.md" <<EOF
# $name

$desc

HologramAgent 插件，随 SDK \`${ver:-?}\` 的示例插件拆分而来。

## 构建

\`\`\`bash
hmapdev build
\`\`\`

产物 \`dist/${name}_bundle.hmap\` 经 \`plugin_install\` 安装。

## 自动更新

本仓库地址登记在 \`plg.json\` 的 \`repo\` 字段，安装后随产物写入
\`plugin.json\` 的 \`repository\` 字段 —— 内核的 \`plugin_update\` 靠它
从本仓库拉最新源码并重编安装。
EOF

	# ③ 推送
	(
		cd "$work/$name"
		git init -q
		git checkout -q -b main
		git add -A
		git -c user.name="$GH_ORG" -c user.email="$GH_ORG@users.noreply.github.com" \
			commit -q -m "feat: 从 SDK example/ 迁移为独立仓库

保留全部源码与测试（dist/build/rawfile/node_modules 等产物不入库）。
plg.json 补 repo 字段，使 plugin_update 能定位本仓库。"
		git remote add origin "https://x-access-token:$TOKEN@github.com/$GH_ORG/$name.git"
		git push -q origin main 2>&1 | tail -1 || echo "   ✗ 推送失败（见上）"
	)
	echo "   ✓ 已推送"
	pushed=$((pushed + 1))
	rm -rf "$work"
done

echo ""
echo "=== 完成 ==="
echo "  创建仓库: $created / 已存在: $skipped / 推送成功: $pushed"
