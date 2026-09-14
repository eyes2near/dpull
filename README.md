# dpull — 多线程 + 断点续传的 Docker 镜像下载器

[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
![ci](https://github.com/eyes2near/dpull/actions/workflows/ci.yml/badge.svg)

面向中国大陆的不稳定网络：把一个镜像的每一层切成固定大小的分片并发下载，
中断后重跑同一条命令即可从断点继续，全部数据按 manifest 里的 sha256 逐层校验，
最后直接 `docker load` 进本机 Docker（也可以只导出 tar / OCI layout / 再推送到你自己的仓库）。

```
             ┌──────────────┐   manifest / manifest list（按平台选层）
 IMAGE ──────▶│ registry 客户端 │──▶ HEAD/Range 探测（大小、是否支持分片）
  (镜像源列表) │  token 鉴权     │
             └──────┬───────┘
                    ▼
        ┌───────────────────────┐   N 个连接并行，边下边写 parts/
        │  分片调度 + 断点续传    │   断线自动重连，未收完的分片从已收字节处续传
        └───────────┬───────────┘
                    ▼
        ┌───────────────────────┐   收齐即校验 sha256（与下载并行）
        │  缓存 ~/.cache/dpull   │──▶ blobs/sha256/<hex>
        └───────────┬───────────┘
                    ▼
     docker-archive(tar) ──▶ docker load   |   OCI layout   |   push 到目标仓库
```

## 安装

```bash
make install                   # → ~/.local/bin/dpull 并在任意 shell 可用（免 sudo）
make build                     # → 只编译到 bin/dpull，不安装
make dist                      # 交叉编译 mac/linux × arm64/amd64 到 dist/
```

只依赖 Go 工具链和标准库，无第三方依赖；产物是单个静态二进制。

## 快速开始

```bash
dpull pull nginx:1.27                       # 下载并 docker load
dpull pull redis:7 -c 16 --chunk 16Mi       # 16 并发、16MiB 分片
dpull pull postgres:16 -o pg16.tar --no-load  # 只导出 tar，稍后拷给别人 docker load
dpull pull registry.k8s.io/pause:3.10 -p linux/amd64
dpull sources                                       # 内置备用源 + 你的 mirror 一起实测排名
dpull pull golang:1.23 --push registry.cn-hangzhou.aliyuncs.com/me/golang:1.23
```

被 Ctrl-C 打断、被 GFW 断线、机器断电之后：**重新执行同一条命令即可**，
已经下完的分片不会重复传输（进度条会提示「已命中缓存/断点 N 个对象」）。

## 它解决的具体问题

| 症状 | dpull 的做法 |
| --- | --- |
| 单连接慢 | 每层按 `--chunk`（默认 8MiB）切片，`-c`（默认 8）个连接并行拉；总并发受控，不会被源站限流打爆 |
| 下到 80% 断线，只能从头再来 | 每个分片落地成 `parts/<digest>/NNNNNN.part` + `.ok` 校验标记；重启后先扫描已完成分片，未收完的分片用 `Range` 从已收字节处继续 |
| 连接挂死不报错 | 每次读流带 stall 看门狗（默认 30s 无字节即判定断线并重连），指数退避 + 抖动重试，尊重 `Retry-After` |
| 镜像源抽风返回半截数据 | 收齐后按 manifest 声明的 sha256 校验；不匹配自动整层重传一次，仍失败则明确报错，绝不产出坏 tar |
| 镜像源声称支持 Range 其实不支持 | 探测 + 运行时识别（用整块响应填空分片会写坏数据），自动降级成单连接整块下载 |
| DNS 被污染 / IPv6 黑洞 | 自实现拨号器：IPv4 优先、每个地址独立超时、失败自动换下一个 IP；**系统解析的地址全连不上时自动用 DoH 重新解析**（绕过被劫持的 UDP/53）；也可 `--resolve 域名=IP` 手工钉死 |
| 只有 docker pull 能用，脚本没法用 | `-q --json` 输出机器可读结果；`--dry-run` 只列层与大小 |

## 命令行

```
dpull [参数] 镜像 [镜像...]        等价于 dpull pull ...
dpull sources [镜像]              实测内置备用源/你的 mirror，按延迟排名并校验内容一致性
dpull bench 镜像                  逐个端点实测速度并排名
dpull prune [--days 7]            删除过期缓存（也可用 DPULL_PRUNE_DAYS）
dpull version
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-c, --concurrency` | `8` | 总并发连接数。带宽足可到 16–24；被源限速反而慢时降到 4 |
| `--chunk` | `8Mi` | 分片大小。层越大可以开越大；`4Mi` 更适合丢包严重的链路 |
| `-p, --platform` | 本机架构 | `linux/amd64`、`linux/arm64/v8`… |
| `--mirror` | 无 | 加速地址，可重复；顺序即优先级，失败自动切下一个，最后回退官方源 |
| `--cache` | `~/.cache/dpull` | 缓存与断点目录 |
| `-o, --output` | 见下 | docker-archive 写 tar 路径；`--format oci` 时写目录 |
| `--format` | `docker-archive` | `docker-archive` \| `oci` \| `none` |
| `--no-load` | 关 | 只下载打包，不执行 `docker load`（默认会导入） |
| `--keep-archive` | 关 | 导入后仍保留 tar（给了 `-o` 时默认保留） |
| `--push` | 无 | 拉完再推送到指定仓库，可重复；推送的是所选平台的单平台 manifest |
| `--retries` | `6` | 单个分片最大重试次数 |
| `--stall` | `30s` | 读流停顿多久判定断线重连 |
| `--days` | `7` | `prune` 子命令删除多少天以前的缓存 |
| `--force` | 关 | 忽略缓存重新下载 |
| `--verify-cached` | 关 | 复用缓存 blob 时重新计算 sha256 |
| `--check-diffids` | 关 | 导入前逐层解压比对 `rootfs.diff_ids`（最严格） |
| `--ipv4-first` | 开 | 优先 IPv4；IPv6 更通畅时 `--ipv4-first=false` |
| `--resolve 域名=IP` | 无 | 绕过污染 DNS，可重复 |
| `--proxy URL` | 跟随环境变量 | 走代理：`http://` `https://` `socks5://` `socks5h://` `socks4://` `socks4a://`，支持 `user:pass@`；`direct` = 忽略 `HTTP_PROXY` |
| `--no-auto-source` | 关 | 关闭内置备用源，只走官方源与你给的 mirror |
| `--prefer-builtin` | 关 | 先试内置备用源再试官方源 |
| `--doh URL` | 内置 4 个 | 自定义 DoH（dns-json）地址，可重复；默认 `doh.pub → alidns → cloudflare → google` |
| `--no-doh` | 关 | 关闭 DoH 兜底解析 |
| `--plain-http HOST` | 无 | 允许指定地址走 http（如 `192.168.1.5:5000`），可重复 |
| `--insecure` | 关 | 允许**本机 loopback** 的 registry 走 http（不会波及公网源站） |
| `--skip-tls-verify` | 关 | 跳过 TLS 证书校验（自签证书仓库） |
| `--user` / `--password` | 无 | registry 凭据；也读 `DPULL_USERNAME/DPULL_PASSWORD` 与 `~/.docker/config.json`（含 credsStore helper） |
| `-q, --quiet`、`--json`、`--dry-run` | 关 | 精简 / 机器可读 / 只列计划 |

环境变量：`DPULL_PROXY`、`DPULL_MIRRORS`（逗号分隔）、`DPULL_CONCURRENCY`、`DPULL_CHUNK`、`DPULL_CACHE`、
`DPULL_PLATFORM`、`DPULL_RETRIES`、`DPULL_PRUNE_DAYS`、`DPULL_DEBUG=1`（打印鉴权与拨号细节）。

## 实测（本机，mcr.microsoft.com，2026-09-13）

| 场景 | 命令 | 结果 |
| --- | --- | --- |
| 链路正常，单连接 | `-c 1 --chunk 1Gi`（73.9MiB 镜像） | 5.4s |
| 链路正常，多分片并行 | `-c 24 --chunk 2Mi` | 5.0s |
| 链路劣化时段，单连接 | `-c 1 --chunk 1Gi` | 232s（≈0.32MiB/s） |
| 链路劣化时段，多分片并行 | `-c 12 --chunk 4Mi` / `-c 24 --chunk 2Mi` | 62s / 4s |
| 313.6MiB 镜像下载中途 Ctrl-C 后续传 | 11 层，中断时磁盘上有 9.2MiB 分片 | 重跑后 44s 完成剩余部分，已收分片 0 次重传 |
| 中断后缓存命中（全量已下） | 重跑同一条命令 | 1.2s（只重新打包 tar，网络请求 0 次） |

解读：链路本身健康时，并行带来的差别不大；**分片并行的真正收益出现在单连接被限速/丢包的时候**
（上表劣化时段相差 2～58 倍），而断点续传让"失败重来"的代价从"整镜像重下"变成"补齐缺的几百 KB"。
上面每一组结论都有对应的自动化测试（`TestRetriesResumeInsideChunk`、`TestResumeAcrossRunsKeepsProgress`、
`TestForceIgnoresCache`），可离线重复。

## 内置备用源：官方源不通就自动换源

不用先找加速地址。**直接 `dpull pull nginx:1.27`，官方源连不上时会自动改用内置备用源**，并在日志里说明换了哪个、为什么：

```
  · registry-1.docker.io 不可达，正在尝试内置备用源…
  · 已从 内置源 1panel 取到 manifest（官方源 registry-1.docker.io 不可达：...connection reset by peer）
平台 linux/arm64/v8，7 层，共 65.7MiB
完成 8/8 层，共 65.7MiB，用时 5s，平均 13.1MiB/s
```

内置清单（2026-09-13 从大陆网络实测，`dpull sources` 可随时重测）：

| 源 | 地址 | 是什么 | 实测 |
| --- | --- | --- | --- |
| `1panel` | `docker.1panel.live` | 公开 Docker Hub 缓存 | 15.6MiB / 5s，最快 |
| `daocloud` | `docker.m.daocloud.io` | DaoCloud 公开加速，需 token | 15.6MiB / 5s |
| `ecr` | `public.ecr.aws/docker/...` | **AWS 官方** Docker Hub 透传副本，路径加 `docker/` 前缀 | 15.6MiB / 6s，匿名有速率限制 |
| `xuanyuan` | `docker.xuanyuan.me` | 公开 Docker Hub 缓存 | 15.6MiB / 16s，免费额度易限流 |

`docker.1ms.run` 也能拉通但同一镜像要 3m46s，**故意没放进清单**。

三条设计上的硬规则，避免「自动换源」变成「自动被坑」：

1. **只在网络类错误时换源**。`manifest unknown`、`401 unauthorized`、`404` 这类是 registry 的正式回答，绝不重试到别的源上——否则你打错的 tag 会被某个缓存解析成另一个构建产物。
2. **换源后依然按该源 manifest 里的 sha256 逐层校验**，并且 `dpull sources` 会跨源比对 manifest digest，不一致会明确报警：
   ```
   内容一致性: 3 个源返回同一 manifest sha256:45e09956dc667c5 ✅
   ```
3. **记住上次赢的源**（`<缓存目录>/sources.json`，72 小时有效），下次不再先浪费 10 秒撞死掉的官方源。

相关参数：

| 参数 | 作用 |
| --- | --- |
| `dpull sources [镜像]` | 实测官方源 + 你的 `--mirror` + 内置源，按延迟排序，附内容一致性校验 |
| `--mirror URL` | 你自己的加速地址，优先级最高；给了它就不会先撞官方源 |
| `--no-auto-source` | 完全关闭内置源（排查内容差异时用） |
| `--prefer-builtin` | 先试内置源再试官方源（官方源被限速但还能回话时有用） |
| `~/.docker/daemon.json` 的 `registry-mirrors` | dpull 会自动读，Docker 自己也用同一份配置 |

优先级：`--mirror` → `daemon.json` 的 mirrors → 内置备用源 → 官方源（`--prefer-builtin` 时内置源排在最前）。

> 第三方缓存本质上是中间人，可信度来自内容校验而不是它的名字。要绝对确信，就拿官方渠道给出的 digest 对一次：`dpull sources` 里 `ecr` 那一行是 AWS 官方副本，用它做交叉印证最稳。

## 用代理：`--proxy`

代理是**唯一能同时解决 DNS 污染和 SNI 干扰**的办法（DoH/`--resolve` 只能解决前者）。

```bash
# HTTP / SOCKS5 混合端口（Clash 一类工具的默认 7890）
dpull pull nginx:1.27 --proxy http://127.0.0.1:7890

# 纯 SOCKS5 端口（常见 1080 / 10808），大陆网络推荐 socks5h
dpull pull nginx:1.27 --proxy socks5h://127.0.0.1:1080

# 带认证
dpull pull nginx:1.27 --proxy socks5://me:s3cret@192.168.1.20:1080

# 临时不用代理（哪怕环境变量里有个失效的 HTTPS_PROXY）
dpull pull nginx:1.27 --proxy direct
```

| 协议 | 谁解析域名 | 说明 |
| --- | --- | --- |
| `http://` `https://` | 本机 | 对 https 目标走 `CONNECT`；`https://` 指代理自身用 TLS |
| `socks5://` | **本机** | 本机 DNS 被污染时，解析仍会被污染（dpull 的 DoH 兜底在这里也生效） |
| `socks5h://` | **代理端** | 域名原样交给代理解析 —— 大陆网络推荐这个 |
| `socks4://` | 本机 | 只支持 IPv4 目标 |
| `socks4a://` | 代理端 | 同上，域名交给代理 |

行为细节：

- **本机 registry 永远不走代理**（`localhost` / `127.0.0.1`），所以全局挂了代理也不会把 `localhost:5000` 送去绕一圈。
- 不写 `--proxy` 时照旧遵守 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`；写了就以它为准。
- 代理自己也走同一套解析链路，所以**代理域名被污染时也能靠 DoH 连上**。
- 代理挂了会立刻停下并给出针对性建议，不会把剩下所有源都试一遍浪费你时间。
- 也可以用环境变量 `DPULL_PROXY` 固定。

> **一个必须知道的坑**：代理能不能绕过 SNI 干扰，取决于流量是不是从**境外节点**出去。如果代理软件对该域名走 `DIRECT`（直连规则），TLS 的 ClientHello 还是从你本机发出、SNI 照样被看到、照样被 RST。把 docker 相关域名设成走节点，或直接开 TUN 模式。
> 本机验证的是「代理链路接通、数据正确、校验通过」，境外出口的实际抗干扰效果需要你有真实节点时再验一次。

## DNS 被污染了怎么办（本机实测结论）

同一台机器上实测，Docker Hub 拉不动其实是**两层叠加**的问题，要分开看：

| 层次 | 现象 | 能否靠客户端解决 |
| --- | --- | --- |
| **DNS 污染** | `registry-1.docker.io` 被解析到黑洞地址（A `128.242.250.155`、AAAA `2001::6ca0:a262`）；连直接向 `8.8.8.8` / `223.5.5.5` 发 UDP/53 查询也返回同样的黑洞答案 —— 上游 53 端口被透明劫持 | ✅ 能。DoH 拿到的是真答案（`doh.pub` 给出 `3.225.102.148` 等 AWS 真实 IP）。dpull 默认在「系统解析的地址全连不上」时自动 DoH 重解析并打印提示 |
| **SNI / IP 层干扰** | 用真实 IP 直连时 TLS 握手被 `connection reset`；同一批 IP 过一会儿又能返回 401 | ❌ 客户端解决不了。只能 `--mirror` 加速地址，或 `--proxy` 走代理 |

三步自查（可直接抄，能明确区分上面两层）：

```bash
dscacheutil -q host -a name registry-1.docker.io                       # 1. 系统给的答案
curl -s "https://doh.pub/dns-query?name=registry-1.docker.io&type=A"    # 2. DoH 给的真答案
curl -s --resolve registry-1.docker.io:443:<真IP> -o /dev/null \
     -w "%{http_code}\n" https://registry-1.docker.io/v2/               # 3. 用真 IP 直连
```

第 3 步返回 `401` = 只有 DNS 被污染，dpull 的 DoH 兜底就能搞定；
报 `TLS connection reset` = SNI 干扰，必须换加速地址或代理（`dpull` 也遵守 `HTTP_PROXY`/`HTTPS_PROXY`）。

要让**整台机器**的 DNS 干净（而不只是 dpull），三选一：

1. **最省事**：不动 DNS，只用镜像加速地址 —— 写进 `~/.docker/daemon.json` 的 `registry-mirrors`，dpull 会自动读取，Docker 本身也受益。
2. **本地干净解析器**：用 mihomo / sing-box / AdGuard Home / NextDNS profile 之类把系统 DNS 指到 `127.0.0.1`，上行走 DoH/DoT（本机实测 853 端口是通的）。注意只改路由器 DNS 没用，UDP/53 会被劫持。
3. **走代理**：`dpull pull 镜像 --proxy http://127.0.0.1:<port>`（或 `socks5h://…`），污染和 SNI 干扰一起解决。`docker` 本身不认 `--proxy`，要让它也走代理就 `export HTTPS_PROXY=…`，两者都吃这个变量。注意代理对该域名不能走直连规则，见上一节。

## 安装到任意 shell

```bash
make install          # 默认装到 ~/.local/bin/dpull（已在 PATH 上，无需 sudo）
make install PREFIX=/usr/local/bin   # 装到系统目录（该目录需存在且可写，可能要 sudo）
make uninstall        # 移除
```

`~/.local/bin` 由 `~/.zshrc` 加入 PATH；bash 登录 shell 走 `~/.profile`，非登录交互走
`~/.bashrc`，都已覆盖。装完直接：

```bash
dpull pull nginx:1.27
dpull sources
```

> 改了源码要重新 `make install`，否则 shell 里用的还是旧副本（Agent Skill 里已内置
> "源码更新就自动重装"的判断）。

## 给 Agent 用：内置 Skill

仓库自带一个 Agent Skill（[.agents/skills/dpull/](.agents/skills/dpull/SKILL.md)），让 agent 遇到
「镜像拉不动」时知道要用 dpull、按退出码判断成败、知道怎么分诊，而不是反复重试
`docker pull` 或自己去 curl registry API。

```bash
# 装到全局（真实目录 + 软链文件，改仓库文档不会漂移）
mkdir -p ~/.agents/skills/dpull
R="$PWD/.agents/skills/dpull"
ln -sfn "$R/SKILL.md" ~/.agents/skills/dpull/SKILL.md
ln -sfn "$R/references" ~/.agents/skills/dpull/references
```

结构（渐进式披露：只有关键词命中时 agent 才展开正文）：

```
.agents/skills/dpull/
├── SKILL.md                        触发条件、定位二进制、默认命令、退出码、失败分诊表
└── references/troubleshooting.md   DNS 污染 vs SNI 干扰的三步自查、日志行解读、选源信任、代理坑、参数速查
```

已验证：新开会话问「nginx:1.27 用 docker pull 一直超时」，agent 自动加载本 skill 并给出
`"$DPULL" pull nginx:1.27 --json`（而不是重试 docker pull）。

按 Agent Skills 标准编写，pi / Claude Code / Codex 等同类 harness 可直接用同一份目录，
或在其 settings 里把 `.agents/skills` 加进 skills 搜索路径。

## 缓存目录结构

```
~/.cache/dpull/
├── blobs/sha256/<hex>        # 已校验的完整 blob（跨镜像复用，相同层只下载一次）
├── parts/sha256-<hex>/       # 断点：NNNNNN.part 数据 + NNNNNN.ok(size/sha256) + meta.json(分片计划)
└── archives/                 # 未指定 -o 时临时导出的 tar，导入成功后自动删除
```

`meta.json` 记录了分片大小与总数，改了 `--chunk` 会作废旧分片（这是唯一会让断点失效的参数改动）。

每个 blob 在开始下载前会多发一个 `Range: bytes=0-0` 的探测请求：Azure/MCR 这类源站的 HEAD 不带
`Accept-Ranges`，但 GET 是正确返回 206 的，如果只看 HEAD 就会误判成"不支持分片"，整张镜像会悄悄退化成
每层一条连接（实测慢 3～50 倍）。

## 国内网络建议

1. **先别急着想加速地址**：直接 `dpull pull 镜像`，官方源不通会自动改用内置备用源；想看清有哪些源可用就 `dpull sources`。
2. **加速地址从哪来**：云厂商容器镜像服务控制台都会给一个专属加速域名（阿里云 ACR「镜像工具 → 镜像加速」、
   腾讯云 TCR、华为云 SWR 等），学校/运营商也常有公共 mirror。填到 `~/.docker/daemon.json`
   的 `registry-mirrors` 里 dpull 会自动读取，也可以命令行 `--mirror` 临时指定。
   注意：绝大多数加速地址只代理 Docker Hub，拉 `registry.k8s.io`、`ghcr.io` 时需要用支持该域名的代理。
3. **匿名拉 Docker Hub 撞 429**：登录（`--user/--password`）或换加速地址，dpull 会按 `Retry-After` 退避。
4. **一次拉取、多处使用**：`dpull pull xxx -o xxx.tar --no-load`，tar 拷到内网机器 `docker load -i xxx.tar`。
5. **中转为自己的镜像**：`--push registry.cn-xxx.aliyuncs.com/ns/app:tag`，内网/构建机直接从自己仓库拉。
6. **报错时**：dpull 会针对 DNS、429、401、校验失败、docker 未启动等情况给出具体的下一步建议。

## 已实现 / 暂不支持

已实现：Docker Registry v2 + OCI 分发鉴权（含 token、Basic、identity token、docker credsStore helper）、
manifest list/index 按平台选择、按 digest 引用、多镜像批量、多镜像源故障转移、Range 分片并行、
分片内续传、sha256 校验、docker-archive / OCI layout 导出、`docker load`、blob 上传与 manifest 推送、
DoH 兜底解析、测速、缓存清理。

端到端已验证（colima + Docker 29.5 真实守护进程）：`pull → docker load → docker run`、
`pull → push 到本地 registry → 再 pull → load → run`、多平台镜像按 `-p` 选层、
中断续传后导入。真实镜像：`mcr.microsoft.com/dotnet/{runtime-deps,runtime}:8.0`、
`public.ecr.aws/docker/library/registry:2`。

暂不支持（用到时请注意）：
- `schema1` 老 manifest；非 `layers` 类型的 rootfs；foreign/非分发层的多平台镜像。
- `--push` 只推所选平台的 manifest（不会重建 manifest list），跨仓库 mount 只在同仓库生效。
- 镜像源不支持 `Range` 时该层无法续传（只能整块重来）——这是协议限制，dpull 会明确提示。
- `docker load` 需要本机 Docker 守护进程在跑；只想拿 tar 用 `--no-load`。

## 开发

```bash
make lint     # gofmt + go vet
make test     # 单元测试
make race     # 并发测试（-race，重复 3 轮）
```

```
cmd/dpull/            命令行：参数解析、提示文案、人类可读的尺寸/时长
internal/reference/   镜像引用解析（tag / digest / 私库端口 / 官方镜像补 library/）
internal/registry/    registry 客户端：鉴权、镜像源故障转移、Range 读、blob 上传
internal/xfer/        分片调度、续传、stall 看门狗、重试退避、进度渲染
internal/store/       缓存布局、分片落地与校验、装配、清理
internal/archive/     docker-archive 与 OCI layout 写出
internal/app/         端到端编排 + 假 registry 测试（含随机断流、错误数据、push 收端）
```

测试里最关键的一条是 **`TestPullWritesValidDockerArchive`**（假 registry 会随机断流、返回错误数据、假装支持 Range）：它校验 tar 里的层顺序、文件名与
`rootfs.diff_ids` 逐一对应 —— 这正是 `docker load` 的判定条件（见 moby `image/tarexport/load.go`
的 `archive.DecompressStream` + diffID 比对），因此不依赖本机 Docker 也能证明产物可导入。

## CI

每次 push / PR 都会跑（`linux` 与 `macos` × `go 1.22.x` 与 `stable` 四个组合）：

```
gofmt 检查 → go vet → go test -race ./...
                     └─ 交叉编译 4 个平台并上传构建产物
```

打 tag（`git tag v1.0.1 && git push --tags`）会自动编译 4 个平台并挂到 GitHub Release；
也可以在 Actions 页面手动触发一次同样的构建。测试全部离线自测（假 registry：随机断流、
谎报 Range、返回错误数据、push 收端），不依赖外网，所以 CI 结果不会因为网络而抖动。

## 许可

MIT © [eyes2near](https://github.com/eyes2near) —— 见 [LICENSE](LICENSE)。
随便用、随便改、随便商用，保留版权声明即可。
