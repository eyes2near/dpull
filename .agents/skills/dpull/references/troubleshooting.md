# dpull 排障参考（实测得出，不是抄的通用建议）

## 先分清两层故障

在这类网络里，"拉不动"通常是**两件独立的事叠在一起**，修法完全不同：

| 层 | 干了什么 | 谁能解决 |
| --- | --- | --- |
| DNS 污染 | 把域名解析成黑洞 IP（常见特征：A 记录落在 `128.242.x` / `31.13.x`，AAAA 出现 `2001::xxxx` 这种明显假地址） | dpull 自动 DoH 兜底 / `--resolve` |
| SNI 干扰 | 拿到真 IP 后，TLS `ClientHello` 里的明文域名命中名单，直接伪造 RST 掐断 | **只有** `--mirror` 或 `--proxy`（且代理要走境外出口） |

### 三步自查（能明确区分两层）

```bash
dscacheutil -q host -a name registry-1.docker.io                        # 1 系统给的答案
curl -s "https://doh.pub/dns-query?name=registry-1.docker.io&type=A"     # 2 DoH 给的答案
curl -s --resolve registry-1.docker.io:443:<真IP> -o /dev/null \
     -w "%{http_code}\n" https://registry-1.docker.io/v2/                # 3 用真 IP 直连
```

第 3 步返回 **401** → 只是 DNS 污染，dpull 的 DoH 兜底就能解决。
第 3 步报 **connection reset** → SNI 干扰，客户端层面**无解**，必须 `--mirror` 或 `--proxy`。

注意：`UDP/53` 可能被透明劫持，**把 DNS 改成 8.8.8.8 / 223.5.5.5 在这类网络是无效的**。
DoH 服务器本身也可能被定向污染（实测同一台 doh.pub 对 docker 域名给假 IP、对
mcr.microsoft.com 给真 IP），所以 dpull 内置了多个 DoH 并允许 `--doh` 覆盖。

## 关键日志行怎么读

| dpull 输出 | 含义 | 下一步 |
| --- | --- | --- |
| `系统解析结果不可用（…），DoH 重新解析到 … 等 N 个地址，已绕过被污染的 DNS` | DNS 层已解决 | 若整体成功就别管了 |
| `改用 DoH 解析到 … 后仍连不上：要么解析本身还在被定向污染，要么是 SNI/IP 层干扰` | 需要 mirror 或代理 | `--mirror` / `--proxy` |
| `不可达，正在尝试内置备用源…` → `已从 内置源 xxx 取到 manifest` | 已自动换源成功 | 汇报时说明来源 |
| `内置源 xxx 失败: … TOOMANYREQUESTS` | 公开源限流 | `sources` 换一个，或让用户给 mirror |
| `所有请求经由 <url> 出站` | 正在走代理 | 确认代理出口是否境外 |
| `<mode> 代理握手失败：对端没有按 SOCKS 协议回应` | 把 HTTP 代理端口写成了 `socks5://` | 改 `--proxy http://…` |

## 选源与信任

```bash
"$DPULL" sources <镜像>            # 人类可读排名 + 内容一致性结论
"$DPULL" sources <镜像> --json     # 机器可读：name/base/ok/seconds/speed/digest/error
```

- 内置源清单会过期，**永远以 `sources` 的实时结果为准**，不要相信记忆里的地址。
- 内置源**按上游分组**：`docker.io` 与 `gcr.io` / `registry.k8s.io` / `ghcr.io` / `quay.io` /
  `mcr.microsoft.com` / `nvcr.io` 各有各的改写缓存，跨上游不通用（`sources --json` 的 `note` 字段会写明）。
- `sources` 会比对各源返回的 manifest digest。多个源给出同一 digest 才是有效印证；
  只有一个源可用时，无法交叉印证——需要明确告诉用户这个风险。
  特例：`gcr.io` 在大陆整个域不可达，无法和官方源对账，只有两家不同运营方的缓存互相印证，
  比“和官方对账”弱一档，汇报时要如实说明。
- `内置:ecr` 是 `public.ecr.aws/docker/...`，AWS 官方透传副本，做交叉印证最稳，
  但匿名访问有速率限制。
- 第三方缓存（1panel / daocloud / xuanyuan 一类）本质是中间人：内容靠 digest 保证，
  但"用哪个源的 manifest"仍由它决定，要如实告知用户。

## 代理

```bash
--proxy http://127.0.0.1:7890        # Clash 混合端口（HTTP + CONNECT）
--proxy socks5h://127.0.0.1:1080     # 域名交给代理解析，大陆网络推荐
--proxy socks5://u:p@host:1080       # 带认证
--proxy direct                       # 忽略 HTTP_PROXY / HTTPS_PROXY
```

- 本机 registry（`localhost` / `127.0.0.1`）永远不走代理，不用为它配 `NO_PROXY`。
- 代理域名自己也走 DoH 兜底链路。
- **代理能否绕开 SNI，取决于是否从境外节点发出**：本机代理若对该域名走 `DIRECT`
  规则，`ClientHello` 仍从本机发出、SNI 仍明文可见 → 依然被 RST。让用户把 docker
  域名设为走节点，或开 TUN 模式。
- 代理不通时 dpull 会立刻停止遍历备用源（换第四个源不会更好），直接给出自检命令。

## 续传与缓存

- 默认缓存在 `~/.cache/dpull`：`blobs/sha256/`（成品层）、`parts/sha256-<hex>/`
  （分片与 `.ok` 标记）、`archives/`（导出的 tar）。
- 重跑**完全相同的命令**即续传；中断（含 SIGINT / 超时 / 断网）不丢已完成分片。
- `--chunk` 变更会使已有分片失效重下；其余参数随意改。
- `--keep-parts` 保留分片（占双份空间），`--force` 忽略缓存重下。
- 空间不足：`"$DPULL" prune --days 3`；先 `du -sh ~/.cache/dpull` 取证。

## 验证真的可用

```bash
docker images | grep <镜像名>
docker run --rm <镜像> sh -c 'echo ok'
# 层内容核验（最严格，慢）：拉时加 --check-diffids
```

导出物：

- `docker-archive`（默认）：`docker load -i img.tar`。
- `--format oci`：目录里有 `oci-layout` / `index.json` / `blobs/`，镜像名沿用短名
  （如 `alpine:3.20`）。已验证可被 skopeo 直接读并转换：

```bash
skopeo inspect oci:./img.oci:alpine:3.20                       # 看 digest/arch/层
skopeo copy oci:./img.oci:alpine:3.20 docker-archive:/tmp/x.tar  # 转成可 docker load 的 tar
```

## 参数速查

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-c/--concurrency` | 8 | 总分片并发；网络差反而要调小到 4 |
| `--chunk` | 8Mi | 分片大小；改了会重下 |
| `-p/--platform` | 本机 | `linux/amd64`、`linux/arm64/v8` |
| `--mirror` | 无 | 加速地址，顺序即优先级 |
| `--no-auto-source` | 关 | 禁用内置源 |
| `--prefer-builtin` | 关 | 内置源排在官方源之前 |
| `--proxy` | 跟随环境变量 | 见上 |
| `--doh` / `--no-doh` | 内置 4 个 | DoH 服务器 |
| `--resolve H=IP` | 无 | 手工钉死解析 |
| `--retries` / `--stall` | 6 / 30s | 分片重试 / 读流停滞判定 |
| `--plain-http` / `--insecure` / `--skip-tls-verify` | 关 | 自建仓库 |
| `--load` / `--no-load` | load | 是否导入 Docker |
| `--format` | docker-archive | `docker-archive` / `oci` / `none` |
| `-q` / `--json` / `--dry-run` | 关 | 机器友好输出 |

环境变量：`DPULL_PROXY` `DPULL_MIRRORS` `DPULL_CONCURRENCY` `DPULL_CHUNK`
`DPULL_CACHE` `DPULL_PLATFORM` `DPULL_RETRIES` `DPULL_USERNAME` `DPULL_PASSWORD`
`DPULL_DEBUG=1`（打印每次拨号与端点决策）。
