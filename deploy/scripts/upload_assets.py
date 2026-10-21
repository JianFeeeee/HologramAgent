#!/usr/bin/env python3
"""上传 release 资产到 gitcode（两步：取签名 URL → PUT 到 OBS）。

用法: upload_assets.py <tag> <token> [file...]
不传 file 时上传 dist/release/ 下全部发布产物。

环境变量：
  GITCODE_REPO  目标仓库，默认 JianFeeeee/HomeAgent（SDK 仓传 JianFeeeee/homeagent-sdk）
                这是 gitcode 镜像仓名的**唯一定义处**（workflow 里通过 job 级
                env.GC_REPO 传入同一个值）。改 gitcode 仓名时改这里 + 两个
                workflow 的 GC_REPO，共三处（每个文件各一个定义点）。
                取值依据（实测 2026-10-21）：gitcode 上 JianFeeeee/HomeAgent
                返回 200，JianFeeeee/HologramAgent 返回 404 —— GitHub 主仓
                已改名 HologramAgent，但 gitcode 侧镜像仓未跟着改。
  ASSET_DIR     资产目录，默认 <repo>/dist/release

为何两步：gitcode 的 release 附件不走 API 直传，而是先向
`releases/<tag>/upload_url` 要一个 OBS 预签名 URL（带 x-obs-* 回调头），
再把文件 PUT 到那个 URL。回调头必须原样透传，否则 OBS 收下了文件但
gitcode 侧不会登记为 release 附件。
"""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

REPO = os.environ.get("GITCODE_REPO", "JianFeeeee/HomeAgent")
API = "https://gitcode.com/api/v5/repos"

# 发布产物后缀。注意 Windows 安装器是 HomeAgent_v*_win64.exe，
# 与 bin/ 里的裸 .exe 靠 _win64.exe 后缀区分。
ARTIFACT_SUFFIXES = (
    ".tar.gz",
    ".zip",
    ".deb",
    ".rpm",
    ".pkg",
    "_win64.exe",
    # 插件包。之前不在白名单里，会被静默跳过——而 release 本该带上它们，
    # 否则用户要自己装 Go + hmapdev 逐插件构建（见 SDK 仓 scripts/build_plugin_bundles.sh）。
    ".hmap",
    # 插件包汇总校验和（与 SHA256SUMS 同性质，独立文件免得混淆内核包与插件)
    "SHA256SUMS.plugins",
)


def is_artifact(name: str) -> bool:
    return name == "SHA256SUMS" or name.endswith(ARTIFACT_SUFFIXES)

def get_upload_url(tag: str, token: str, filename: str) -> tuple[str, dict]:
    q = urllib.parse.urlencode({"file_name": filename})
    url = f"{API}/{REPO}/releases/{tag}/upload_url?{q}"
    req = urllib.request.Request(url, headers={"private-token": token})
    with urllib.request.urlopen(req, timeout=30) as r:
        data = json.loads(r.read())
    return data["url"], data.get("headers", {})


def put_file(url: str, headers: dict, path: str) -> tuple[int, str]:
    size = os.path.getsize(path)
    with open(path, "rb") as f:
        body = f.read()
    req = urllib.request.Request(url, data=body, method="PUT")
    for k, v in headers.items():
        req.add_header(k, v)
    req.add_header("Content-Length", str(size))
    try:
        # 大文件（Full 变体安装包近 100MB）给足超时。
        with urllib.request.urlopen(req, timeout=900) as r:
            return r.status, r.read().decode("utf-8", "replace")[:300]
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")[:300]
    except Exception as e:  # noqa: BLE001
        return 0, f"{type(e).__name__}: {e}"


def project_root() -> str:
    """向上找带 go.mod 的目录作为仓库根。

    为何不数 dirname：本脚本初版在 scripts/（深度 1），移到 deploy/scripts/
    （深度 2）后写死的两层 dirname 就指向了 deploy/dist/release，上传直接
    FileNotFoundError。这正是 v0.7.2 那次 package/ → deploy/packaging/ 打断
    PROJECT_ROOT 的同一个坑，改成按标记文件定位以后怎么挑位置都不会错。
    """
    d = os.path.dirname(os.path.abspath(__file__))
    while d != os.path.dirname(d):
        if os.path.exists(os.path.join(d, "go.mod")):
            return d
        d = os.path.dirname(d)
    # 实在找不到（脚本被单独拷出仓库）就回退到 cwd，给 ASSET_DIR 一个机会
    return os.getcwd()


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    tag, token = sys.argv[1], sys.argv[2]
    outdir = os.environ.get("ASSET_DIR") or os.path.join(
        project_root(), "dist", "release"
    )
    if not os.path.isdir(outdir):
        print(f"error: 资产目录不存在: {outdir}")
        print("       用 ASSET_DIR=<目录> 显式指定，或先跑构建生成 dist/release/")
        return 2
    files = sys.argv[3:] or sorted(
        f for f in os.listdir(outdir) if is_artifact(f)
    )
    if not files:
        print(f"error: {outdir} 下没有可识别的发布产物")
        return 2
    print(f"repo={REPO} tag={tag} dir={outdir}", flush=True)
    failed = []
    for name in files:
        path = os.path.join(outdir, name)
        if not os.path.isfile(path):
            print(f"skip (missing): {name}", flush=True)
            continue
        mib = os.path.getsize(path) / 1048576
        print(f"==> {name} ({mib:.1f} MiB)", flush=True)
        try:
            url, headers = get_upload_url(tag, token, name)
        except Exception as e:  # noqa: BLE001
            print(f"    upload_url FAILED: {e}", flush=True)
            failed.append(name)
            continue
        status, body = put_file(url, headers, path)
        ok = 200 <= status < 300
        print(f"    PUT -> {status} {'OK' if ok else body}", flush=True)
        if not ok:
            failed.append(name)
    print(f"\n{'ALL OK' if not failed else f'{len(failed)} FAILED: ' + ', '.join(failed)}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
