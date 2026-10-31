# 站点基础设施运维手册

> **状态：操作手册（现行）。** 站点基础设施的操作说明。

> 适用：本项目**文档站与产品站**的生产部署（多站点、统一入口、多层 TLS 终止）。
> 读者：后续维护者 / AI agent。
> **本文件不含任何密钥、token、真实域名与 IP** —— 全部用占位符，取值见本机 `deploy/` 外的私有笔记或基础设施面板。
>
> 相关：部署脚本见 [`deploy/`](../../deploy/)；分支与发布流程见 [`docs/git-branching.md`](../git-branching.md)。

---

## 0. 一句话拓扑

```
用户 ──443──▶ <PUBLIC_IP>（公网入口机）
                 └─ 反向代理 / WAF          ← ★ TLS 在这里终止（第一层）
                      ├─ 默认站点：<WILDCARD_CERT>（两级通配，覆盖 *.example.com）
                      └─ 每条三级子域一个 drop-in（专属证书 + 精确 server_name）
                           └─ 隧道 / 上行到内网
                                └─ 内网网关机:3080（第二层 TLS 终止）
                                     └─ 各静态站根目录 /app/data/sites/<name>/
```

**关键认知**：流量要穿**两层** TLS 终止。改一层不等于改完 —— 这是本手册多数坑的根源。

---

## 1. 部署一个静态站（照抄流程）

以新增 `foo.example.com` 为例，共四步。

### 1.1 站点文件

静态产物放到内网网关机的站点根：

```bash
# 产物目录名即站名，与 nginx drop-in 里的 root 对应
sudo mkdir -p <SITES_ROOT>/foo
sudo tar xzf foo-site.tar.gz -C <SITES_ROOT>/foo
sudo find <SITES_ROOT>/foo -type f | wc -l    # 核对文件数
```

### 1.2 内部层：nginx drop-in

⚠️ **不要改主配置** —— 它由管理后台生成，会被覆盖。正确做法是加一个**独立文件**：

```bash
sudo tee <GATEWAY_DATA>/gateway/zz-foo.conf >/dev/null <<'EOF'
server {
    listen 3080 ssl http2;
    listen 8443 ssl http2;
    server_name foo.example.com;

    ssl_certificate     "/etc/nginx/certs/foo.example.com.fullchain.pem";
    ssl_certificate_key "/etc/nginx/certs/foo.example.com.privkey.pem";
    ssl_protocols TLSv1.2 TLSv1.3;

    root /app/data/sites/foo;
    index index.html;

    auth_request off;           # 公开站点；需鉴权则删掉此行
    location / {
        auth_request off;
        # ★ 纯静态多页站必须用 =404，不能回落 index.html
        #   回落会让不存在的路径返回首页 + 200（假装成功，最难查的一类问题）
        try_files $uri $uri/ =404;
    }
    location = /index.html { auth_request off; add_header Cache-Control "no-store"; }

    gzip on;
    gzip_types text/css application/javascript application/json image/svg+xml text/markdown;
}
EOF
docker exec <GATEWAY_CONTAINER> nginx -t && docker exec <GATEWAY_CONTAINER> nginx -s reload
```

> 为什么用 drop-in：主配置由管理后台生成（标注「勿手改」），但 nginx 侧是
> `include <confdir>/*.conf`，独立文件既不被覆盖也不污染生成件。**回滚 = 删文件 + reload。**

### 1.3 证书（三级子域必须单独签）

**两级通配证书不覆盖三级子域**，且上层反代的域名白名单也只支持通配（同样只到两级），
所以 `foo.example.com` 这类**必须单独签发**。

```bash
acme.sh --issue --dns dns_ali -d foo.example.com --server letsencrypt   # 或 zerossl
acme.sh --install-cert -d foo.example.com --ecc \
  --key-file       <CERT_DIR>/foo.example.com.privkey.pem \
  --fullchain-file <CERT_DIR>/foo.example.com.fullchain.pem \
  --reloadcmd      "sudo /opt/acme_reload_hook.sh"      # 见 §3
```

### 1.4 公网层：专属证书 + drop-in

公网入口机与内网机**可能是两台机器**（本项目即如此）。公网机上：

```bash
# ① 证书放到公网机的证书目录
#    宿主 <WAF_DATA>/resources/nginx ≡ 容器内 /etc/nginx
# ② 加一个与内网 drop-in 同构的 server 块，但多一段「上行到隧道」：
cat > <WAF_DATA>/resources/nginx/conf.d/foo-bypass.conf <<'EOF'
upstream foo_backend {
    server 127.0.0.1:<TUNNEL_VHOST_PORT>;
    keepalive 32;
    keepalive_timeout 75;
}

server {
    listen 0.0.0.0:443 ssl;
    server_name foo.example.com;

    ssl_certificate     /etc/nginx/certs/foo.example.com.crt;
    ssl_certificate_key /etc/nginx/certs/foo.example.com.key;

    location / {
        proxy_pass https://foo_backend;
        proxy_set_header Host              $http_host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_ssl_server_name on;
        proxy_ssl_name $host;      # ★ 保住 SNI，否则上游证书校验会错
        proxy_buffering off;
        proxy_request_buffering off;
        chunked_transfer_encoding on;
        # ★ 关闭缓冲攒包：否则小响应体攒不满就不下发（首屏空白类问题）
        postpone_output 0;
        tcp_nopush off;
        tcp_nodelay on;
        sendfile off;
    }
}
EOF
docker exec <WAF_CONTAINER> nginx -t && docker exec <WAF_CONTAINER> nginx -s reload
```

> nginx 里**精确 `server_name` 优先于通配/默认站点**，所以加这个块只影响本域名。

---

## 2. 验收（必须走真实路径）

### 2.1 为什么不能用本机 curl 直接验

内网 DNS 常把 `*.example.com` 解析到**内网机**。此时本机 curl 测的是内网那条路，
**公网层的问题一个都测不出来** —— 本项目就因此误报过「已修好」两次。

**正确做法**：强制把域名指向目标 IP 再验。

```bash
# 指定走公网入口机
curl -s -o /dev/null -w "%{http_code} verify=%{ssl_verify_result}\n" \
  --resolve foo.example.com:443:<PUBLIC_IP> https://foo.example.com/
# 期望：200 verify=0

# 看实际下发的证书
echo | openssl s_client -servername foo.example.com -connect <PUBLIC_IP>:443 2>/dev/null \
  | openssl x509 -noout -subject -dates
```

浏览器验证（跟随真实证书链、不跳校验）：

```javascript
// Playwright：用 host-resolver-rules 把域名钉到公网 IP
// ★ ignoreHTTPSErrors 必须为 false —— 跳过了就等于没测
const b = await chromium.launch({
  args: [`--host-resolver-rules=MAP foo.example.com <PUBLIC_IP>`],
});
const ctx = await b.newContext({ ignoreHTTPSErrors: false });
```

### 2.2 验收清单

- [ ] 域名走公网 IP：`verify=0`
- [ ] 证书 CN 是**该子域自己**，不是通配 / 默认证书
- [ ] 证书未过期（`openssl x509 -noout -enddate`）
- [ ] 不存在的路径返回 **404**（不是 200 + 首页）
- [ ] 真实浏览器无 SSL 错误
- [ ] 多视口无横向溢出、无控制台错误
- [ ] 站内链接与锚点全部可达

---

## 3. 证书续期（全自动）

### 3.1 流程

```
acme.sh cron（每日一次）
  └─ 到期前 30 天自动续期（LE 现用 ARI 建议窗口）
       └─ 续期成功 → --reloadcmd: /opt/acme_reload_hook.sh
            ├─ ① 内网 nginx reload（本机立刻用上新证书）
            └─ ② 推送到公网入口机（先 dry-run 校验，通过才真推）
```

钩子**始终 exit 0**：公网推送失败只落日志告警，不把 acme 的续期标记为失败
（内网证书已装好，那是两件事）。日志：`/var/log/push-cert.log`。

### 3.2 推送到公网机的方式

公网机通常 **SSH 不通**，走**云厂商的「运行命令」API**（本项目用阿里云云助手）：

```python
# 思路（脱敏伪码）：复用现成封装，别自己重写签名
sys.path.insert(0, '/opt')
import push_cert            # 内含 AK/实例 ID 与调用封装，import 时 __main__ 保护
status, output = push_cert.call('RunCommand', {...})
```

⚠️ **`CommandContent` 有体积上限**：本项目实测 3 份证书 base64 后约 13KB 会被拒，
单份约 6.5KB 正常。**逐条推送**，不要合并。

### 3.3 ★ 重写推送脚本时必须保留的四道防线

本项目曾因缺这些防线造成**全站 TLS 故障**（详见 §5.1）。新版 `push_cert.py` 的设计：

| # | 防线 | 作用 |
|---|---|---|
| 1 | **绝不使用 glob** | 证书目录名可能含字面 `*`，glob 会误匹配到别的证书 |
| 2 | **显式映射表**（源路径 → 目标文件 → **期望 CN**）| 没在表里就不推 |
| 3 | **本地断言**：CN / SAN / 私钥配对 / 未过期 | 写之前就拦住 |
| 4 | **远端复核**：写完用 openssl 验 CN，不符即退出 | 最后一道保险 |

外加默认 `--dry-run`（只校验不打印敏感内容、不写文件）。

### 3.4 手动验证 install + reload 链路（不消耗签发额度）

`--install-cert` 只重装**本地已签**的证书并跑钩子；配合 `--force` 才是真重签。

```bash
stat -c '%y' <CERT_DIR>/foo.example.com.fullchain.pem   # 记下 mtime
acme.sh --install-cert -d foo.example.com --ecc \
  --key-file <CERT_DIR>/foo.example.com.privkey.pem \
  --fullchain-file <CERT_DIR>/foo.example.com.fullchain.pem \
  --reloadcmd "sudo /opt/acme_reload_hook.sh"
stat -c '%y' <CERT_DIR>/foo.example.com.fullchain.pem   # mtime 应更新
```

---

## 4. 管理后台接口（改配置的正确方式）

部署在本机的管理后台用 host 路由 + admin token 暴露配置接口。

### 4.1 先取自描述清单

```bash
A=http://127.0.0.1:<ADMIN_PORT>/api     # 容器内直连；外部走 /admin/api
curl -s "$A/schema"                     # ★ 别猜字段名
```

`schema` 会列出**所有可配置项**（含当前为空的）与各自的写入口、请求体示例。

### 4.2 常用写入

```bash
TOK=<ADMIN_TOKEN>
# 站点文案
curl -s -X POST -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"title":"...","portalTitle":"..."}' "$A/pageinfo"

# 路由表（★ 整表替换 → 必须先 GET 现状，改完 POST，再 apply）
curl -s -H "Authorization: Bearer $TOK" "$A/gateway/routes"   # 先读全表
curl -s -X POST -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"routes":[ ...完整列表... ]}' "$A/gateway/routes"
curl -s -X POST -H "Authorization: Bearer $TOK" "$A/gateway/apply"   # ★ 不 apply 不生效

# 导航条目（★ 嵌套形状，扁平形状会被拒）
curl -s -X POST -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"sectionIndex":N,"item":{"title":"...","url":"...","icon":"auto"}}' "$A/item"
```

### 4.3 三条硬规矩

1. **路由表是整表替换**：必须提交完整 `{"routes":[...]}`。只提交单条会把整张表换掉。
2. **写配置后要验证，不能以接口 200 当成功**：改完轮询探测目标 URL
   （配置应用是**异步**的，立刻探测会拿到旧结果，表现为「apply 了却 404」）。
3. **系统管理的分组不要手工加条目**（本项目里是「常用服务」「历史访问」）——
   它们由程序按点击/浏览记录重建，加进去会被下次重建挤掉，还会污染排序。
   要加就加到用户分组。

---

## 5. 已知陷阱与事故复盘

### 5.1 ★ 字面 `*` 交给 glob → 写坏全局默认证书（本项目真实事故）

**经过**：推送通配证书的脚本写了
`glob.glob('/root/.acme.sh/*.example.com_ecc/fullchain.cer')`。
而 **acme.sh 存放通配证书的目录名，字面上就叫 `*.example.com_ecc`（星号是真实字符）**。
glob 把它当通配符展开，同时匹配到 `foo.example.com_ecc`，且返回顺序不定 ——
`[0]` 拿到别的证书，写进了**全局默认证书**，导致所有走默认证书的域名同时 TLS 报错。

**教训**：

- **字面 `*` 绝不能交给 glob**；读证书一律用**字面路径** `open()`（Python 的 open 不展开通配）。
- **写生产前必须断言「读到的是什么」** —— 校验 CN 成本极低，漏了就是全站故障。

### 5.2 只测内网 → 误报「已修好」

见 §2.1。**验收必须走真实路径。**

### 5.3 删证书/配置记录前先查引用

删 acme 记录或配置前，**grep 全盘引用**（含 `/opt/` 这类容易漏的地方）：

```bash
grep -rln "<证书目录名>" /etc/nginx/ /opt/ <GATEWAY_DATA>/ /home/ 2>/dev/null
```

本项目曾删掉一条证书记录后，才发现 `/opt/` 下的推送脚本正指向它。

### 5.4 nginx 的 mime.types 没有 `.md`

给 agent 直读的 Markdown 会以 `application/octet-stream` 下发，部分客户端拒收。
显式声明：

```nginx
location ~* \.md$ { default_type text/markdown; try_files $uri =404; }
location ~* \.txt$ { default_type text/plain;  try_files $uri =404; }
# 并把这些类型加进 gzip_types
```

### 5.5 静态站不要回落 index.html

`try_files $uri $uri/ /index.html` 会让**不存在的路径返回首页 + 200**，
监控和链接检查都会「通过」。纯静态多页站一律：

```nginx
try_files $uri $uri/ =404;
```

### 5.6 反代要关缓冲攒包

上游反向代理默认 `postpone_output 1460`，小响应体攒不满就不下发（首屏空白）。
旁路静态站时统一关掉（见 §1.4 的 server 块）。

---

## 6. 为 agent 提供直读入口（推荐）

纯 HTML 站点对 agent 不友好（样板占大头、易漏内容）。推荐随构建产出：

| 路径 | 内容 |
|---|---|
| `/llms.txt` | 目录：每页一行，带 URL 与一句话说明 |
| `/llms-full.txt` | 全部正文拼一份，一次读完（记得开 gzip） |
| `/<page>.md` | 每页 Markdown 原文（`text/markdown`）|

⚠️ 两个必踩的坑：
1. **静态站点生成器通常只把 `.md` 渲染成 HTML，不复制原文** ——
   需在构建流程里额外复制一份，否则 `llms.txt` 里的链接全 404。
2. nginx 需显式声明 `.md`/`.txt` 的类型（见 §5.4）。

---

## 7. 脱敏约定（写文档/脚本时遵守）

**绝不写进仓库**：

| 类别 | 示例形态 | 替代写法 |
|---|---|---|
| 真实域名 | `*.example.com` | `<域名>` / `foo.example.com` |
| 公网 IP | 任意公网地址 | `<PUBLIC_IP>` |
| 内网 IP | 任意内网地址 | `<LAN_IP>` / `<GATEWAY_IP>` |
| token / 密钥 | 各类 admin/sync token、云 AK/SK | `<ADMIN_TOKEN>` 等占位符 |
| 资源 ID | 云主机实例 ID | `<INSTANCE_ID>` |
| 目录/容器名 | 具体部署路径 | `<SITES_ROOT>` / `<GATEWAY_DATA>` 等 |

**取值放哪**：本机私有笔记、密码管理器、基础设施面板 —— 不进版本库。
脚本里通过**环境变量**读取，不硬编码：

```python
AK_ID = os.environ['ALI_AK_ID']          # 而不是字面量
```
