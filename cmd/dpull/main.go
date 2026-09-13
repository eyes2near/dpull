// Command dpull downloads Docker/OCI images with many parallel connections and
// resumes interrupted transfers, then loads them into the local Docker engine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dpull/internal/app"
	"dpull/internal/xfer"
)

const version = "1.0.0"

// listFlag collects repeated flag values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) > 0 {
		switch argv[0] {
		case "-v", "--version", "version":
			fmt.Printf("dpull %s\n", version)
			return 0
		case "-h", "--help", "help":
			usage("")
			return 0
		}
	}

	o, sub, err := parseFlags(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		usage("")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "pull":
		results, err := app.Pull(ctx, o)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "\n已中断。已下载的分片都保留在缓存目录，重新执行同一条命令即可从断点继续。")
				printResults(o, results)
				return 130
			}
			fmt.Fprintln(os.Stderr, "\n失败:", err)
			fmt.Fprintln(os.Stderr, "已下载的分片保留在缓存目录，重新执行同一条命令即可从断点继续。")
			advice(err, o).print()
			printResults(o, results)
			return 1
		}
		printResults(o, results)
		if ctx.Err() != nil {
			return 130
		}
		return 0
	case "bench":
		if err := app.Bench(ctx, o); err != nil {
			fmt.Fprintln(os.Stderr, "测速失败:", err)
			return 1
		}
		return 0
	case "prune":
		days := o.PruneDays
		freed, err := app.Prune(o.CacheDir, days)
		if err != nil {
			fmt.Fprintln(os.Stderr, "清理失败:", err)
			return 1
		}
		fmt.Printf("已清理 %s 的旧缓存（%d 天以前）\n", xfer.HumanBytes(freed), days)
		return 0
	default:
		usage(sub)
		return 2
	}
}

// explicit reports whether any of the named flags was actually passed.
func explicit(fs *flag.FlagSet, names ...string) bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

// advice turns raw registry/network errors into the next thing worth trying.
type adviceBox struct {
	lines []string
}

func (a adviceBox) print() {
	for _, l := range a.lines {
		fmt.Fprintln(os.Stderr, "建议:", l)
	}
}

func advice(err error, o *app.Options) adviceBox {
	msg := strings.ToLower(err.Error())
	var out adviceBox
	add := func(s string) { out.lines = append(out.lines, s) }
	switch {
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "connection reset"), strings.Contains(msg, "no such host"),
		strings.Contains(msg, "tls handshake"), strings.Contains(msg, "context deadline"):
		add("这个端点在当前网络不可达。先测速再挑可用端点: dpull bench " + firstImage(o) + " --mirror <加速地址>")
		if len(o.Mirrors) == 0 {
			add("没有 --mirror 时只能直连官方源，大陆网络通常需要在 ~/.docker/daemon.json 的 registry-mirrors 里配置加速地址，或显式加 --mirror")
		}
		if o.PreferIPv4 {
			add("已优先尝试 IPv4；若本机 IPv6 反而通畅，可加 --ipv4-first=false")
		}
		if strings.Contains(msg, "no such host") {
			add("DNS 解析异常时可用 --resolve 域名=IP 固定解析结果")
		}
		add("网络不稳定可提高容错: -c 4 --chunk 4Mi --retries 10 --stall 20s")
		if strings.Contains(msg, "reset by peer") || strings.Contains(msg, "tls handshake") {
			add("若日志里出现过「DoH 重新解析…」说明 DNS 污染已绕过，剩下的是 SNI/IP 层干扰：只能换 --mirror 加速地址，或设置 HTTPS_PROXY 走代理")
		}
	case strings.Contains(msg, "429") || strings.Contains(msg, "too many requests"):
		add("触发速率限制了：用 --user/--password 登录，或改用云厂商的镜像加速地址")
	case strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(msg, "unauthorized"):
		add("需要登录私有仓库: --user 用户名 --password 密码（或先 docker login 复用本机凭据）")
	case strings.Contains(msg, "digest") || strings.Contains(msg, "校验") || strings.Contains(msg, "mismatch"):
		add("校验不通过说明镜像源数据有损坏，重试或换源: --force --mirror <另一个加速地址>")
	case strings.Contains(msg, "docker load"):
		add("镜像已下载完成，只是导入本机 docker 失败：确认 Docker 已启动（docker info），或加 --no-load -o 文件.tar 稍后手动 docker load")
	case strings.Contains(msg, "manifest") && strings.Contains(msg, "404"):
		add("该仓库里没有这个 tag：确认 tag 拼写，或确认这个加速地址是否代理了该域名（多数加速地址只代理 Docker Hub）")
	}
	return out
}

func firstImage(o *app.Options) string {
	if len(o.Images) > 0 {
		return o.Images[0]
	}
	return "alpine:3.20"
}

func hasProgress(o *app.Options) bool { return !o.Quiet }

func printResults(o *app.Options, results []app.Result) {
	if o.Quiet || o.JSON || len(results) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "\n摘要:")
	for _, r := range results {
		fmt.Fprintf(os.Stderr, "  %s  平台 %s  层 %d  大小 %s  用时 %s\n",
			r.Image, r.Platform, r.Layers, xfer.HumanBytes(r.Bytes),
			(time.Duration(r.Seconds) * time.Second).Round(time.Second))
		if r.Loaded {
			fmt.Fprintln(os.Stderr, "    已导入本机 Docker")
		}
		if r.Archive != "" {
			fmt.Fprintf(os.Stderr, "    归档文件 %s\n", r.Archive)
		}
		if r.OCILayout != "" {
			fmt.Fprintf(os.Stderr, "    OCI layout %s\n", r.OCILayout)
		}
		for _, p := range r.Pushed {
			fmt.Fprintf(os.Stderr, "    已推送 %s\n", p)
		}
	}
}

func parseFlags(argv []string) (*app.Options, string, error) {
	sub := "pull"
	if len(argv) > 0 {
		switch argv[0] {
		case "pull", "bench", "prune":
			sub = argv[0]
			argv = argv[1:]
		}
	}
	o := &app.Options{}
	fs := flag.NewFlagSet(sub, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var mirrors, tags, pushes listFlag
	var noLoad bool
	var resolves, doh, plainHTTP listFlag
	fs.StringVar(&o.Platform, "platform", envStr("DPULL_PLATFORM", ""), "目标平台，如 linux/amd64、linux/arm64/v8（默认跟随本机 CPU）")
	fs.StringVar(&o.Platform, "p", envStr("DPULL_PLATFORM", ""), "--platform 的简写")
	fs.IntVar(&o.Concurrency, "c", envInt("DPULL_CONCURRENCY", 8), "并发连接数（总线程数）")
	fs.IntVar(&o.Concurrency, "concurrency", envInt("DPULL_CONCURRENCY", 8), "并发连接数")
	fs.StringVar(&o.ChunkSizeS, "chunk", envStr("DPULL_CHUNK", "8Mi"), "单个分片大小，如 4Mi/16Mi/1M")
	fs.Var(&mirrors, "mirror", "镜像加速地址，可重复或逗号分隔，按顺序尝试")
	fs.StringVar(&o.CacheDir, "cache", envStr("DPULL_CACHE", defaultCacheDir()), "缓存/断点目录")
	fs.StringVar(&o.Output, "o", "", "输出文件（docker-archive 的 tar 路径，或 --format oci 时的目录）")
	fs.StringVar(&o.Output, "output", "", "--output 的长写法")
	fs.StringVar(&o.Format, "format", "docker-archive", "输出格式: docker-archive | oci | none")
	fs.BoolVar(&o.Load, "load", true, "下载完成后导入 docker")
	fs.BoolVar(&noLoad, "no-load", false, "不导入 docker（等价 --load=false）")
	fs.Var(&pushes, "push", "把拉到的镜像再推送到指定仓库，可重复，如 registry.example.com/ns/app:v1")
	fs.IntVar(&o.Retries, "retries", envInt("DPULL_RETRIES", 6), "单个分片最大重试次数")
	fs.StringVar(&o.StallS, "stall", "30s", "读流停顿多久判定为断线并重连")
	fs.BoolVar(&o.Force, "force", false, "忽略缓存，重新下载")
	fs.BoolVar(&o.KeepParts, "keep-parts", false, "组装后保留分片文件（占双份空间）")
	fs.BoolVar(&o.KeepArchive, "keep-archive", false, "导入 docker 后仍保留导出的 tar 文件")
	fs.BoolVar(&o.VerifyCached, "verify-cached", false, "复用缓存时重新计算 sha256")
	fs.BoolVar(&o.CheckDiffIDs, "check-diffids", false, "导入前逐层解压校验 diffID（最严格，最慢）")
	fs.BoolVar(&o.Quiet, "q", false, "只输出关键信息与最终结果")
	fs.BoolVar(&o.Quiet, "quiet", false, "--quiet 的长写法")
	fs.BoolVar(&o.DryRun, "dry-run", false, "只列出镜像层与大小，不下载")
	fs.BoolVar(&o.Insecure, "insecure", false, "允许本机 loopback 的 registry 走 http（其他地址请用 http:// 前缀或 --plain-http）")
	fs.Var(&plainHTTP, "plain-http", "允许走 http 的指定地址，如 192.168.1.5:5000，可重复")
	fs.BoolVar(&o.SkipTLS, "skip-tls-verify", false, "跳过 TLS 证书校验（自建仓库用）")
	fs.BoolVar(&o.PreferIPv4, "ipv4-first", true, "优先连接 IPv4（IPv6 被污染/不通时必须保持）")
	fs.Var(&resolves, "resolve", "域名固定 IP，形如 registry-1.docker.io=104.18.0.1，可重复")
	fs.Var(&doh, "doh", "自定义 DoH 地址（dns-json），可重复；默认内置 doh.pub/alidns/cloudflare/google")
	fs.BoolVar(&o.NoDoH, "no-doh", false, "关闭 DoH 兜底解析（默认：系统解析的地址全连不上时自动启用）")
	fs.StringVar(&o.DockerBin, "docker", envStr("DPULL_DOCKER_BIN", "docker"), "docker 可执行文件路径")
	fs.IntVar(&o.PruneDays, "days", envInt("DPULL_PRUNE_DAYS", 7), "prune 子命令：删除多少天以前的缓存")
	fs.Var(&tags, "tag", "导入 docker 时使用的标签，可重复（默认沿用镜像名）")
	fs.BoolVar(&o.JSON, "json", false, "以 JSON 输出结果")
	fs.StringVar(&o.Username, "user", "", "registry 用户名")
	fs.StringVar(&o.Password, "password", "", "registry 密码")
	fs.Usage = func() { usage(sub) }

	if err := fs.Parse(reorder(argv, fs)); err != nil {
		return nil, sub, err
	}
	o.Mirrors = append(mirrorEnv(), mirrors...)
	o.Tags = tags
	o.PushTargets = pushes
	o.Resolves = resolves
	o.DoH = doh
	o.PlainHTTP = plainHTTP
	if len(fs.Args()) == 0 && sub != "prune" && sub != "bench" {
		return o, sub, errors.New("请指定要下载的镜像，例如 dpull pull nginx:1.27")
	}
	o.Images = fs.Args()

	if noLoad {
		o.Load = false
	}
	if explicit(fs, "o", "output") {
		// an explicit -o always stays on disk
		o.KeepArchive = true
	}
	cs, err := parseSize(o.ChunkSizeS)
	if err != nil {
		return nil, sub, fmt.Errorf("--chunk: %w", err)
	}
	o.ChunkSize = cs
	st, err := parseDuration(o.StallS)
	if err != nil {
		return nil, sub, fmt.Errorf("--stall: %w", err)
	}
	o.Stall = st
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	if o.Concurrency > 64 {
		o.Concurrency = 64
	}
	switch o.Format {
	case "docker-archive", "oci", "none":
	default:
		return nil, sub, fmt.Errorf("未知 --format %q", o.Format)
	}
	if o.Output == "" {
		switch o.Format {
		case "docker-archive":
			o.Output = ""
		case "oci":
			if len(o.Images) > 0 {
				o.Output = defaultOCIPath(o.Images[0])
			}
		}
	}
	if sub == "bench" && o.Concurrency < 4 {
		o.Concurrency = 4
	}
	return o, sub, nil
}

// reorder moves flags before positional arguments so that
// "dpull pull nginx:1.27 --dry-run" behaves like
// "dpull pull --dry-run nginx:1.27": Go's flag package stops parsing at the
// first positional argument, which is unfriendly for a CLI.
func reorder(argv []string, fs *flag.FlagSet) []string {
	var flags, args []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") || a == "-" || len(a) == 1 {
			args = append(args, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.Index(name, "="); eq >= 0 {
			flags = append(flags, a)
			continue
		}
		f := fs.Lookup(name)
		flags = append(flags, a)
		// bool flags do not consume the next argument
		if !boolLike(f) {
			if i+1 < len(argv) {
				i++
				flags = append(flags, argv[i])
			}
		}
	}
	return append(flags, args...)
}

type boolFlag interface {
	IsBoolFlag() bool
}

// boolLike reports whether the flag can be given without a value.
func boolLike(f *flag.Flag) bool {
	if f == nil {
		return false
	}
	bf, ok := f.Value.(boolFlag)
	return ok && bf.IsBoolFlag()
}

func mirrorEnv() []string {
	var out []string
	for _, v := range strings.Split(os.Getenv("DPULL_MIRRORS"), ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func defaultCacheDir() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "dpull")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache", "dpull")
	}
	return filepath.Join(".dpull-cache")
}

func defaultOCIPath(image string) string {
	base := filepath.Base(strings.Split(image, ":")[0])
	return filepath.Join(".", base+".oci")
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// parseSize understands 8Mi, 4M, 512k, 1.5G and plain byte counts.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 8 << 20, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "kib"), strings.HasSuffix(s, "ki"):
		mult, s = 1<<10, trimSuffixMulti(s)
	case strings.HasSuffix(s, "mib"), strings.HasSuffix(s, "mi"):
		mult, s = 1<<20, trimSuffixMulti(s)
	case strings.HasSuffix(s, "gib"), strings.HasSuffix(s, "gi"):
		mult, s = 1<<30, trimSuffixMulti(s)
	case strings.HasSuffix(s, "kb"):
		mult, s = 1000, strings.TrimSuffix(s, "kb")
	case strings.HasSuffix(s, "mb"):
		mult, s = 1000*1000, strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "gb"):
		mult, s = 1000*1000*1000, strings.TrimSuffix(s, "gb")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析 %q", s)
	}
	if f <= 0 {
		return 0, errors.New("必须大于 0")
	}
	return int64(f * float64(mult)), nil
}

func trimSuffixMulti(s string) string {
	for _, suf := range []string{"kib", "mib", "gib", "ki", "mi", "gi"} {
		if strings.HasSuffix(s, suf) {
			return strings.TrimSuffix(s, suf)
		}
	}
	return s
}

// parseDuration accepts "30s" as well as bare "30" (seconds).
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 30 * time.Second, nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < time.Second {
		return 0, errors.New("不能小于 1s")
	}
	return d, nil
}

func usage(sub string) {
	fmt.Fprintf(os.Stderr, `dpull %s — 多线程 + 断点续传的 Docker 镜像下载器

用法:
  dpull [参数] 镜像 [镜像...]          等价于 dpull pull ...
  dpull bench 镜像                     测速各镜像加速地址，给出推荐
  dpull prune [--days 7]               清理过期缓存
  dpull version                        显示版本

常用参数:
  -c, --concurrency N   并发连接数，默认 8；网络差时试 16-24
      --chunk SIZE      分片大小，默认 8Mi；大文件建议 8Mi~32Mi
  -p, --platform OS/ARCH[/VARIANT]  指定架构，默认跟随本机
      --mirror URL      镜像加速地址，可重复；顺序即优先级
      --cache DIR       缓存目录，默认 ~/.cache/dpull
  -o, --output PATH     导出路径；不指定时导入 docker 后自动删除
      --format FMT      docker-archive（默认）| oci | none
      --no-load         只下载和打包，不导入 docker
      --push TARGET     拉取后推送到自己的仓库，可重复
      --retries N       分片重试次数，默认 6
      --stall DUR       读流停顿判定断线，默认 30s
      --ipv4-first      优先走 IPv4（默认开）；IPv6 可用时可 --ipv4-first=false
      --resolve H=IP    固定解析结果，绕过被污染的 DNS，可重复
      --no-doh          关闭 DoH 兜底（默认系统解析的地址全连不上时自动用 DoH 重解析）
      --dry-run         只列出层列表与大小
  -q, --quiet           精简输出
      --json            结果以 JSON 输出
      --user/--password registry 凭据（也可用 DPULL_USERNAME/DPULL_PASSWORD）

其他参数:
      --force           忽略缓存重新下载
      --keep-archive    导入 docker 后仍保留导出的 tar
      --keep-parts      组装后保留分片文件（占双份空间，换下次秒装）
      --verify-cached   复用缓存 blob 时重新计算 sha256
      --check-diffids   导入前逐层解压比对 rootfs.diff_ids（最严格）
      --tag T           导入时使用的标签，可重复
      --docker PATH     docker 可执行文件路径
      --days N          prune 子命令：删除多少天以前的缓存
      --insecure        允许本机 loopback 的 registry 走 http
      --plain-http HOST 允许指定地址走 http，可重复
      --skip-tls-verify 跳过 TLS 证书校验（自建仓库）

环境变量: DPULL_MIRRORS / DPULL_CONCURRENCY / DPULL_CHUNK / DPULL_CACHE / DPULL_PLATFORM /
          DPULL_RETRIES / DPULL_PRUNE_DAYS / DPULL_USERNAME / DPULL_PASSWORD / DPULL_DEBUG=1

示例:
  dpull pull nginx:1.27
  dpull pull registry.k8s.io/pause:3.10 -p linux/amd64 -c 16
  dpull pull redis:7 --mirror https://mirror.example.com --mirror https://origin.example.com
  dpull pull postgres:16 -o ./postgres16.tar --no-load
  dpull bench alpine:3.20 --mirror https://mirror.example.com
  dpull pull golang:1.23 --push registry.cn-hangzhou.aliyuncs.com/me/golang:1.23

中断后重新执行同一条命令即可从断点继续；全部数据都会做 sha256 校验。
`, version)
}
