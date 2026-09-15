---
name: dpull
description: 用 dpull 下载/搬运 Docker 镜像——多线程分片、断点续传、自动换源（按上游分组的内置缓存：docker.io / gcr.io / registry.k8s.io / ghcr.io / quay.io / mcr.microsoft.com / nvcr.io）、DoH 绕 DNS 污染、可走代理，最后导入本机 Docker。当用户说"拉个镜像/这个镜像拉不动/docker pull 超时/离线导入镜像/把镜像推到内网仓库/换个架构版本"或遇到 registry-1.docker.io、gcr.io、registry.k8s.io、ghcr.io、quay.io、mcr.microsoft.com 拉取失败时使用。也用于把镜像导出成 tar / OCI layout、跨机器搬运、在代理受限网络里取镜像。禁止在这种网络下直接改用 curl 手撕 registry API 或反复重试 docker pull。
compatibility: macOS/Linux 单二进制，无需 Docker 即可下载与导出；--load 需本机 docker CLI 可用。网络受限时需可访问内置源或用户自备 mirror/代理。本 skill 描述的行为需要 dpull ≥ 1.1.0（ensure-dpull.sh 会校验并升级）：内置改写缓存在 1.1.0 才覆盖 Docker Hub 以外的上游。
---

# dpull：面向劣质网络的镜像下载器

不要自己拼 registry HTTP 调用，也不要反复重试 `docker pull`。**所有镜像拉取走 dpull**：它做分片并行、分片级续传、sha256 校验、官方源不可达时自动换内置源，失败时给出的 `建议:` 行是可直接执行的下一步。

## 0. 确认工具在位（别假设，也别自己拼下载命令）

调本 skill 自带的脚本（路径相对本 SKILL.md 所在目录）：

```bash
bash <本 skill 目录>/scripts/ensure-dpull.sh          # 只探测，不安装
DPULL=$(bash <本 skill 目录>/scripts/ensure-dpull.sh) # rc=0 时 stdout 才是路径
```

**按退出码决定下一步，不要猜：**

| rc | 含义 | 你要做的 |
|---|---|---|
| `0` | stdout 是可用的 dpull 绝对路径 | 之后一律用 `"$DPULL"`，别用裸名 `dpull`（可能不在 PATH） |
| `10` | 能装但没装（**或已装副本低于本 skill 要求的 1.1.0**，stderr 会说「准备升级」） | **先向用户说明**脚本打出的计划（从哪取、装到哪），取得同意后再加 `--install` 重跑 |
| `11` | 获取失败（下载不通 / **sha256 校验不过** / 平台没有对应包） | 把 stderr 原样汇报给用户并**停下**。绝不静默退回 `docker pull` 硬扛，也绝不绕过校验强行装 |

脚本的获取阶梯，以及为什么这么排：

1. 本机已装且版本 ≥ `1.1.0` → 直接用；**源码比二进制新且有 Go → 自动重编译**（旧副本会报「参数错误」）。
2. 在 dpull 源码检出里且有 Go → 源码编译（开发者场景，改完就该生效）。
3. 否则**下载官方 Release 预编译二进制**，`SHA256SUMS` 逐一对账后才落地 —— **不需要 Go**。
   本机有 `gh` 时优先走 gh（它走 api.github.com，比 github.com 稳）。
4. 都不行才克隆源码编译。

网络受限时用 `--proxy URL`（脚本会把它透给 curl，socks/http 都认）；
装到别处用 `--prefix DIR`；钉版本用 `--version v1.0.0`；镜像前缀用 `--base URL`
（**镜像必须同时转发 SHA256SUMS**，脚本会拒绝拿不到校验和的镜像）。
持久化代理可设 `DPULL_PROXY_URL`，不用每次敲。

装完不在 PATH 上只是提示一句，**不要**据此判断失败，用绝对路径继续。
`sha256 校验不通过` 是**安全事件**，不是网络故障：立刻告诉用户，别再换源重试。

## 1. 默认动作（90% 情况就这一条）

```bash
"$DPULL" pull <镜像> --json
```

不加 `--mirror`、不加 `--proxy`。它会：官方源 → 不通则自动改用内置备用源 → 校验 sha256 → `docker load`。

备用源**按上游分组**，每条只认领自己那个上游：Docker Hub 走 1panel / daocloud / AWS ECR 官方透传副本 / xuanyuan；
`gcr.io`、`registry.k8s.io`、`ghcr.io`、`quay.io`、`mcr.microsoft.com`、`nvcr.io` 也各有两家改写缓存。
所以**不要一上来就要代理或 mirror**：Google 系、k8s 这类看起来「肯定拉不到」的上游，其实默认就能成。

判断成功：**看退出码**，不要看有没有输出。

| 退出码 | 含义 | 该做什么 |
| --- | --- | --- |
| `0` | 成功 | 用 `--json` 里的 `digest` / `loaded` 汇报 |
| `1` | 失败 | 读 stderr 的 `建议:` 行，按 §4 分诊 |
| `130` | 被中断 | 分片已保留，**重跑同一条命令即续传** |
| `2` | 参数错 | 按提示改参数 |

`--json` 输出（stdout，机器可读；进度和诊断都在 stderr）：

```json
[{ "image": "registry-1.docker.io/library/alpine:3.20", "platform": "linux/arm64/v8",
   "digest": "sha256:d9e8...", "tags": ["alpine:3.20"], "layers": 1,
   "bytes": 4092947, "archive": "/path/alpine_3.20.tar", "loaded": true, "seconds": 12.3 }]
```

## 2. 大镜像先估体积，再决定怎么拉

```bash
"$DPULL" pull <镜像> --dry-run          # 只列层与总大小，不下载
```

>500MiB 时给足超时或后台跑，并**降低并发**（网络差时并发越高越容易被掐）：

```bash
"$DPULL" pull <镜像> -c 4 --chunk 4Mi --stall 20s --retries 10
```

超时/中断后**重跑同一条命令**就是续传，不要清缓存、不要加 `--force`。
唯一会破坏续传的参数是 `--chunk`（改了要重下）。

## 3. 常用变体

| 目标 | 命令 |
| --- | --- |
| 只要 tar，不进 Docker | `"$DPULL" pull <镜像> --no-load -o ./img.tar` |
| 导出给 containerd/nerdctl | `"$DPULL" pull <镜像> --format oci -o ./img.oci` |
| 换架构（如在 arm64 上取 amd64） | `"$DPULL" pull <镜像> -p linux/amd64` |
| 拉完转推内网/云仓库 | `"$DPULL" pull <镜像> --push registry.cn-hangzhou.aliyuncs.com/ns/app:v1` |
| 私有仓库凭据 | `--user U --password P`，或事先 `docker login` 复用本机凭据 |
| 批量 | `"$DPULL" pull a:1 b:2 c:3 --json` |
| 自建 http 仓库 | `--plain-http 192.168.1.5:5000`；本机 loopback 用 `--insecure` |
| 清缓存腾空间 | `"$DPULL" prune --days 7` |

凭据与加速地址自动读 `~/.docker/config.json` 与 `~/.docker/daemon.json`，不需要转发给用户。

## 4. 失败分诊（按 stderr 里的关键词，直接照做）

| 现象关键词 | 真实原因 | 下一条命令 |
| --- | --- | --- |
| `i/o timeout` / `no such host` / 解析到 `128.242.x` 或 `2001::` 开头 | DNS 污染 | dpull 已自动 DoH 兜底；仍失败见 §5 |
| `connection reset by peer`（且日志出现过「DoH 重新解析」） | DNS 已绕过，**SNI 干扰** | `"$DPULL" pull <镜像> --proxy socks5h://127.0.0.1:1080` 或 `--mirror <加速地址>` |
| `TOOMANYREQUESTS` / `429` / 「免费节点繁忙」 | 公开源限流 | `"$DPULL" sources <镜像>` 换最快的源，或 `--mirror` 自备地址 |
| `manifest unknown` / 404 | tag 不存在（**不会**换源重试，这是对的） | 找用户确认 tag，别换源绕 |
| `401` / `403` / `unauthorized` | 需要登录 | `--user/--password` 或先 `docker login` |
| `digest` / `mismatch` / 校验不过 | 源数据坏了 | `"$DPULL" pull <镜像> --force --mirror <另一个源>` |
| `gcr.io` / `registry.k8s.io` 整体超时，日志说「DoH 解析后仍连不上」 | 不是 DNS 污染，是 SNI/IP 层封锁，客户端解不了 | **不用你处理**：dpull 会自动改用对应上游的内置缓存，日志出现「已从 内置源 … 缓存（上游 gcr.io）」；两条缓存都失败才走 §5 |
| `代理不可用` | 代理本身没起 | `curl -x <代理> -I https://registry-1.docker.io/v2/` 自检；或 `--proxy direct` |
| `docker load` 失败 | 镜像已下好，只是本机 Docker 没起 | 别重下：`docker info` 检查，或 `--no-load -o ./img.tar` 让用户手动 load |

完整排查（含区分 DNS 污染 / SNI 干扰 / 代理是否走境外出口三步自查命令）见
[references/troubleshooting.md](references/troubleshooting.md)。

## 5. 需要用户决策时，先取证再问

```bash
"$DPULL" sources <镜像> --json     # 实测官方源 + 内置源 + 用户的 mirror
```

按 `seconds` 升序挑 `ok: true` 的项；每行的 `note` 会写明该源代理哪个上游（改写缓存只服务自己那个上游，拿 gcr 的源去拉 quay 不会成功）。`sources` 会跨源比对 manifest digest，
不一致会在人类可读输出里报警。**没有 `ok: true` 的源时，不要瞎重试**，
把结果拿给用户问：要么给一个 `--mirror` 加速地址，要么给一个 `--proxy`。

代理只在流量真的从境外节点出去时才能绕开 SNI 干扰；本机代理对该域名走直连规则时无效。

## 6. 交付前必须自检

```bash
docker images | grep <镜像>                     # 确认真的进了 Docker
docker run --rm <镜像> sh -c 'echo ok'          # 能跑起来才算数（用户要运行时）
```

汇报时说清：**来源**（官方源还是内置源/哪个 mirror）、**平台**、**digest**、
**体积**、**是否已导入 Docker**。来源不是小事——第三方缓存是中间人，可信度来自 digest。

## 不要做的事

- 不要因为"想看看到底通不通"就 `curl https://registry-1.docker.io/v2/` 反复试；用 `"$DPULL" sources`。
- 不要 `rm -rf ~/.cache/dpull` 或加 `--force` 来"重试"——续传是这工具的核心价值。
- 不要为了成功而关掉校验（没有开关可以关，别去改代码）。
- 不要把 `--mirror` 写死成某个第三方源而不告诉用户。
- 不要提“白嫖 GitHub Actions 中转拉镜像”这种方案：实测 GitHub 文件 CDN 回程只有 ~0.5MB/s，比内置源还慢，
  而把 runner 当代理/隧道是违反服务条款的行为。README 里已写明不做，遇到就引用那一节。
