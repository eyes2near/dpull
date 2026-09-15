package app

// Built-in fallback sources.
//
// The problem they solve: on some networks the origin registry is unreachable
// (DNS pollution plus, on top of it, TLS/SNI interference) while a handful of
// public pull-through caches and vendor mirrors are reachable and fast. Rather
// than telling the user to go find a mirror by hand, dpull keeps a small table
// of sources, tries them when the user's own endpoints all failed with a
// *network* error, and remembers which one worked.
//
// The table is keyed by upstream registry: a source only gets tried for the
// references it can actually serve. Docker Hub used to be the only covered
// upstream, which silently left gcr.io / registry.k8s.io / ghcr.io with no
// fallback at all.
//
// Two safety rules:
//
//   - A fallback is only tried for network-class failures. "manifest unknown",
//     "unauthorized" and similar are real answers and must not be retried
//     somewhere else, or a typo'd tag would silently resolve to a different
//     build from a cache.
//   - Whatever source serves the image, every layer is still verified against
//     the digest taken from that source's manifest, and `dpull sources` can
//     cross-check that all sources agree on the manifest digest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dpull/internal/reference"
	"dpull/internal/registry"
	"dpull/internal/xfer"
)

// fallbackSource is one alternate origin.
type fallbackSource struct {
	Name string
	Base string // full base URL including scheme
	// Repo maps the requested repository onto the path this source serves.
	// It reports false when the source cannot possibly serve the reference.
	Repo func(r reference.Ref) (string, bool)
	// Provider and Upstream are display metadata: which operator runs the cache
	// and which registry it proxies. Upstream is empty for the Docker Hub table,
	// whose sources are described by Note instead.
	Provider string
	Upstream string
	Note     string
}

// Label names this source in a log line. Naming the upstream matters: "已从
// daocloud 取到 manifest" does not tell you that a *gcr.io* cache served a
// k8s reference, which is exactly the thing worth seeing.
func (s fallbackSource) Label() string {
	if s.Upstream != "" {
		return fmt.Sprintf("%s 缓存（上游 %s）", s.Provider, s.Upstream)
	}
	if s.Name == "ecr" {
		return "AWS ECR 官方透传副本"
	}
	return s.Name
}

// byName finds a source by its short name (as stored in source memory).
func byName(name string) (fallbackSource, bool) {
	for _, s := range fallbackSources {
		if s.Name == name {
			return s, true
		}
	}
	return fallbackSource{}, false
}

// hubMirror serves Docker Hub repositories under their original path.
func hubMirror(r reference.Ref) (string, bool) {
	if !r.IsDockerHub() {
		return "", false
	}
	return r.Repository, true
}

// ecrAlias maps a Docker Hub repository onto AWS's official pull-through copy
// (public.ecr.aws/docker/...), which is a different network path to the same
// content-addressed blobs.
func ecrAlias(r reference.Ref) (string, bool) {
	if !r.IsDockerHub() {
		return "", false
	}
	return "docker/" + r.Repository, true
}

// hostMirror serves one specific upstream registry with the repository path
// passed through unchanged. Rewrite caches such as gcr.m.daocloud.io are
// pull-through proxies of a *single* origin: only the host differs. Keying on
// the upstream host is what stops one entry from claiming every reference in
// the table and quietly pulling a same-named repository from somewhere else.
func hostMirror(upstreams ...string) func(reference.Ref) (string, bool) {
	return func(r reference.Ref) (string, bool) {
		h := r.Host()
		for _, u := range upstreams {
			if strings.EqualFold(h, u) {
				return r.Repository, true
			}
		}
		return "", false
	}
}

// upstreamNote builds the human note for a rewrite cache.
func upstreamNote(provider, upstream string) string {
	return provider + " 公开加速（上游 " + upstream + "，路径原样）"
}

// fallbackSources is ordered by measured throughput from a mainland-China
// network on 2026-09-13 (redis:7.2-alpine, 15.6MiB, 16 connections):
// 1panel 5s, daocloud 5s, ecr 6s, xuanyuan 16s. docker.1ms.run served the same
// image in 3m46s and is deliberately left out. Public caches come and go, so
// `dpull sources` re-measures them and the run itself always falls through to
// the next candidate on failure.
//
// The first four entries only proxy Docker Hub (see hubMirror/ecrAlias), which
// used to mean every other registry had no fallback at all: `dpull pull
// registry.k8s.io/...` would fail with "内置备用源也没能救回来" while a working
// cache sat two keystrokes away. The rewrite caches below cover the upstreams
// that actually get blocked. Each entry was accepted only after it returned the
// *same manifest digest* as the official registry for the same tag:
//
//	registry.k8s.io/pause:3.10          sha256:ee6521f290b2  官方 = k8s.m.daocloud.io = k8s.1ms.run
//	quay.io/argoproj/argocd:v2.11.0     sha256:e81cfc1f5761  官方 = quay.m.daocloud.io = quay.1ms.run
//	mcr.microsoft.com/dotnet/runtime:8.0 sha256:9cfa8aaf5c98 官方 = mcr.m.daocloud.io = mcr.1ms.run
//	ghcr.io/home-assistant/...:stable   sha256:a1bc133af84e  官方 = ghcr.m.daocloud.io = ghcr.1ms.run
//	nvcr.io/nvidia/cuda:12.4.1-base-...  sha256:0f6bfcbf267e  官方 = nvcr.m.daocloud.io
//	gcr.io/distroless/base:latest       sha256:0ebad3510af5  gcr.m.daocloud.io = gcr.1ms.run
//
// gcr.io is the one row that cannot be compared against the origin from a
// mainland network (it is simply unreachable here), so it stands on two caches
// run by different operators agreeing on the digest — weaker than a comparison
// with the origin, and `dpull sources` says as much by printing every digest it
// saw. Never trust a cache silently: layer bytes are always checked against the
// digest in the manifest the cache itself served.
//
// Rewrite hosts must be probed, not guessed: registry.k8s.m.daocloud.io looks
// like the obvious pattern and is dead, while k8s.m.daocloud.io works.
//
// Order inside one upstream is the first-try preference, measured 2026-09-15
// with `dpull sources` from the same mainland network (延迟 / 首块速度):
//
//	gcr:  daocloud 5.8s 76KiB/s  >  1ms 9.5s 31KiB/s      ← gcr 上 daocloud 明显快
//	mcr:  daocloud 13.1s         >  1ms 13.2s（两条都慢，好在 mcr 官方本身可达）
//	k8s:  daocloud 3.9s 128KiB/s vs 1ms 5.8s 86KiB/s，重跑一次互换（1ms 4.7s/123 对 daocloud 5.4s/95）
//	quay: 1ms 13.2s 78KiB/s      >  daocloud 12.7s 47KiB/s
//	ghcr: 1ms 3.0s               >  daocloud 4.2s（两条都只有个位数 B/s，基本是看谁先响应）
//
// Read that as: only the gcr gap is bigger than the noise, the rest of the pairs
// swap places between runs. These are public caches that churn, so the order
// decides what gets tried first and nothing more — `dpull sources` re-measures,
// and a failing run falls through to the next candidate.

// upstreamSource builds a rewrite-cache entry with its display metadata wired
// up, so the provider and upstream are only typed once per row.
func upstreamSource(name, base, provider, upstream string) fallbackSource {
	return fallbackSource{
		Name:     name,
		Base:     base,
		Repo:     hostMirror(upstream),
		Provider: provider,
		Upstream: upstream,
		Note:     upstreamNote(provider, upstream),
	}
}

var fallbackSources = []fallbackSource{
	// Docker Hub
	{Name: "1panel", Base: "https://docker.1panel.live", Repo: hubMirror, Provider: "1panel", Note: "公开 Docker Hub 缓存"},
	{Name: "daocloud", Base: "https://docker.m.daocloud.io", Repo: hubMirror, Provider: "DaoCloud", Note: "DaoCloud 公开加速"},
	{Name: "ecr", Base: "https://public.ecr.aws", Repo: ecrAlias, Provider: "AWS", Note: "AWS 官方 Docker Hub 透传副本"},
	{Name: "xuanyuan", Base: "https://docker.xuanyuan.me", Repo: hubMirror, Provider: "xuanyuan", Note: "公开 Docker Hub 缓存"},

	// gcr.io — 大陆最常见的第二个死点（Google 系整体不通）
	upstreamSource("daocloud-gcr", "https://gcr.m.daocloud.io", "DaoCloud", "gcr.io"),
	upstreamSource("1ms-gcr", "https://gcr.1ms.run", "1ms", "gcr.io"),

	// registry.k8s.io — 官方端点本身就时通时断，不是单纯的大陆问题。
	// 两条的快慢会在重跑时互换，挑 daocloud 只是因为它缓存命中更常见。
	upstreamSource("daocloud-k8s", "https://k8s.m.daocloud.io", "DaoCloud", "registry.k8s.io"),
	upstreamSource("1ms-k8s", "https://k8s.1ms.run", "1ms", "registry.k8s.io"),

	// ghcr.io — 通但极慢（实测单连接 58KB/s、8 并发 1.2MB/s），缓存不一定更快
	upstreamSource("1ms-ghcr", "https://ghcr.1ms.run", "1ms", "ghcr.io"),
	upstreamSource("daocloud-ghcr", "https://ghcr.m.daocloud.io", "DaoCloud", "ghcr.io"),

	// quay.io / mcr.microsoft.com / nvcr.io — 本身可达，作为限流或被墙时的备份路径
	upstreamSource("1ms-quay", "https://quay.1ms.run", "1ms", "quay.io"),
	upstreamSource("daocloud-quay", "https://quay.m.daocloud.io", "DaoCloud", "quay.io"),
	upstreamSource("daocloud-mcr", "https://mcr.m.daocloud.io", "DaoCloud", "mcr.microsoft.com"),
	upstreamSource("1ms-mcr", "https://mcr.1ms.run", "1ms", "mcr.microsoft.com"),
	upstreamSource("daocloud-nvcr", "https://nvcr.m.daocloud.io", "DaoCloud", "nvcr.io"),
}

// sourceMemoryFile is how long a remembered winner stays preferred. Public
// caches churn quickly, so this is deliberately short.
const sourceMemoryTTL = 72 * time.Hour

// sourceMemory records the last source that actually delivered an image for a
// given origin host.
type sourceMemory struct {
	Origin string    `json:"origin"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

func sourceMemoryPath(cacheDir string) string {
	return filepath.Join(cacheDir, "sources.json")
}

func loadSourceMemory(cacheDir string) map[string]sourceMemory {
	out := map[string]sourceMemory{}
	b, err := os.ReadFile(sourceMemoryPath(cacheDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	for k, v := range out {
		if time.Since(v.At) > sourceMemoryTTL {
			delete(out, k)
		}
	}
	return out
}

func saveSourceMemory(cacheDir, origin, source string) {
	mem := loadSourceMemory(cacheDir)
	mem[origin] = sourceMemory{Origin: origin, Source: source, At: time.Now()}
	b, err := json.MarshalIndent(mem, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return
	}
	tmp := sourceMemoryPath(cacheDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, sourceMemoryPath(cacheDir))
}

// orderedFallbacks returns the built-in sources to try, with a remembered
// winner moved to the front.
func orderedFallbacks(cacheDir string, origin string) []fallbackSource {
	mem := loadSourceMemory(cacheDir)
	win, ok := mem[origin]
	if !ok {
		return fallbackSources
	}
	out := make([]fallbackSource, 0, len(fallbackSources))
	var rest []fallbackSource
	for _, s := range fallbackSources {
		if s.Name == win.Source {
			out = append(out, s)
		} else {
			rest = append(rest, s)
		}
	}
	return append(out, rest...)
}

// networkErrorHints are the substrings that mean "we never got an answer from
// the registry" as opposed to "the registry answered no".
var networkErrorHints = []string{
	"dial tcp", "i/o timeout", "reset by peer", "connection refused", "broken pipe",
	"no route to host", "network is unreachable", "no such host", "tls handshake",
	"unexpected eof", "connection reset", "timeout awaiting", "context deadline exceeded",
	"eof", "server misbehaving", "dns", "can't assign requested address",
}

// isNetworkError reports whether err means the endpoint was unreachable rather
// than refusing the request on the merits.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, h := range networkErrorHints {
		if strings.Contains(msg, h) {
			// An explicit registry answer outranks the noise around it.
			if strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "unauthorized") {
				return false
			}
			return true
		}
	}
	return false
}

// resolveWithSources resolves the manifest through the user's endpoints and, if
// those are unreachable, through the built-in sources. It returns the client
// that succeeded, which later stages must keep using so blobs come from the
// same place the manifest did.
func resolveWithSources(ctx context.Context, o *Options, ref reference.Ref, spec, platform string, preferBuiltins bool) (*registry.Client, *registry.Resolved, error) {
	eps, err := endpointsFor(o, ref)
	if err != nil {
		return nil, nil, err
	}
	cli := newClient(o, eps)

	// chain is the user\'s own endpoint list (explicit mirrors, daemon config,
	// origin). Everything after it is a built-in fallback.
	type attempt struct {
		name string
		base string
		repo string
	}
	chain := attempt{repo: ref.Repository}
	var attempts []attempt
	if !preferBuiltins {
		attempts = append(attempts, chain)
	}
	if !o.NoAutoSource {
		for _, s := range orderedFallbacks(o.CacheDir, ref.Host()) {
			if repo, ok := s.Repo(ref); ok {
				attempts = append(attempts, attempt{"内置:" + s.Name, s.Base, repo})
			}
		}
	}
	if preferBuiltins {
		attempts = append(attempts, chain)
	}

	var chainErr error
	for _, at := range attempts {
		isChain := at.base == ""
		c, epList := cli, eps
		if !isChain {
			ep, err := parseEndpoint(at.name, at.base, o.Insecure, plainHTTPHosts(o))
			if err != nil {
				continue
			}
			epList = []registry.Endpoint{ep}
			c = newClient(o, epList)
		}
		res, err := c.Resolve(ctx, at.repo, spec, platform)
		if err == nil {
			if !isChain {
				why := ""
				if chainErr != nil {
					why = "（官方源 " + ref.Host() + " 不可达：" + trimErr(chainErr) + "）"
				}
				cli.Logf("已从 %s 取到 manifest%s", sourceLabel(at.name), why)
				saveSourceMemory(o.CacheDir, ref.Host(), strings.TrimPrefix(at.name, "内置:"))
			}
			return c, res, nil
		}
		if isChain {
			chainErr = err
			// A deliberate answer (unknown tag, bad credentials) must not be retried
			// through a cache: that masks the real problem and can resolve a typo
			// into some other build. Equally, a dead proxy is a local
			// misconfiguration that no number of sources can work around.
			if o.NoAutoSource || preferBuiltins || errors.Is(err, registry.ErrProxy) || !isNetworkError(err) {
				return nil, nil, err
			}
			cli.Logf("%s 不可达，正在尝试内置备用源…", ref.Host())
			continue
		}
		cli.Logf("%s 失败: %s", sourceLabel(at.name), trimErr(err))
		if errors.Is(err, registry.ErrProxy) {
			// the proxy is down; walking the rest of the table is pointless
			if chainErr == nil {
				chainErr = err
			}
			break
		}
		if chainErr == nil {
			chainErr = err
		}
	}
	if chainErr == nil {
		chainErr = fmt.Errorf("%s: 没有可用的源", ref.Host())
	}
	return nil, nil, chainErr
}

func sourceLabel(name string) string {
	short := strings.TrimPrefix(name, "内置:")
	if s, ok := byName(short); ok {
		return "内置源 " + s.Label()
	}
	return "内置源 " + short
}

// trimErr shortens an error for one-line display.
func trimErr(err error) string {
	if err == nil {
		return "-"
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(s) > 110 {
		// the interesting part (status code, registry error code) is at the end
		s = "..." + s[len(s)-107:]
	}
	return s
}

// sourceRow is one measured line of `dpull sources`.
type sourceRow struct {
	Name    string  `json:"name"`
	Base    string  `json:"base"`
	Note    string  `json:"note,omitempty"`
	OK      bool    `json:"ok"`
	Seconds float64 `json:"seconds"`
	Speed   float64 `json:"speed_bps,omitempty"`
	Digest  string  `json:"manifest_digest,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Err     string  `json:"error,omitempty"`
}

// IsProxyError reports a failure caused by the proxy itself, which the CLI turns
// into proxy-specific advice.
func IsProxyError(err error) bool { return errors.Is(err, registry.ErrProxy) }

// CheckProxy validates a --proxy value before any work starts.
func CheckProxy(raw string) error { return registry.CheckProxy(raw) }

// Sources probes every candidate source for one image and prints what actually
// happened, including whether the sources agree on the manifest digest.
func Sources(ctx context.Context, o *Options, images []string) (int, error) {
	if len(images) == 0 {
		images = []string{"alpine:3.20"}
	}
	probe := images[0]
	ref, err := reference.Parse(probe)
	if err != nil {
		return 0, err
	}
	spec := refSpec(ref)

	var rows []sourceRow
	digests := map[string][]string{}
	try := func(name, base, repo, note string) {
		// Whatever path this probe takes, stamp its note on the row it produces:
		// "内置:daocloud-gcr" alone does not say which upstream it proxies, and
		// the JSON is what agents and scripts read.
		before := len(rows)
		defer func() {
			for i := before; i < len(rows); i++ {
				rows[i].Note = note
			}
		}()
		ep, err := parseEndpoint(name, base, o.Insecure, plainHTTPHosts(o))
		if err != nil {
			rows = append(rows, sourceRow{Name: name, Base: base, Err: err.Error()})
			return
		}
		c := newClient(o, []registry.Endpoint{ep})
		c.Logf = func(string, ...any) {}
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, o.sourceProbeTimeout())
		defer cancel()
		res, err := c.Resolve(cctx, repo, spec, o.Platform)
		if err != nil {
			rows = append(rows, sourceRow{Name: name, Base: ep.Base, Seconds: time.Since(start).Seconds(), Err: classifyProbeErr(err)})
			return
		}
		// Read a real 2 MiB slice of the first layer: answering /v2/ nicely and
		// serving bulk data are two different abilities.
		var got int64
		var speed float64
		if len(res.Layers) > 0 {
			l := res.Layers[len(res.Layers)-1]
			want := int64(2 << 20)
			if l.Size > 0 && l.Size < want {
				want = l.Size
			}
			bstart := time.Now()
			got, err = readRange(cctx, c, res.Repo, l.Digest, want)
			if err != nil && got == 0 {
				rows = append(rows, sourceRow{Name: name, Base: ep.Base, Seconds: time.Since(start).Seconds(), Err: "manifest 可取，但 blob 读取失败: " + classifyProbeErr(err), Digest: manifestDigest(res)})
				return
			}
			speed = float64(got) / time.Since(bstart).Seconds()
		}
		dig := manifestDigest(res)
		if dig != "" {
			digests[dig] = append(digests[dig], name)
		}
		rows = append(rows, sourceRow{
			Name: name, Base: ep.Base, OK: true,
			Seconds: time.Since(start).Seconds(), Speed: speed,
			Digest: dig, Bytes: res.TotalBytes,
		})
	}

	try("origin:"+ref.Host(), ref.Host(), ref.Repository, "")
	for i, m := range o.Mirrors {
		try(fmt.Sprintf("mirror%d", i+1), m, ref.Repository, "")
	}
	for _, s := range dockerDaemonMirrors() {
		try("daemon-mirror", s, ref.Repository, "")
	}
	for _, s := range fallbackSources {
		repo, ok := s.Repo(ref)
		if !ok {
			continue
		}
		try("内置:"+s.Name, s.Base, repo, s.Note)
	}

	if o.JSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return countOK(rows), nil
	}

	fmt.Printf("探测 %s（源越快排越前）\n\n", ref.String())
	head := []string{"源", "地址", "延迟", "首块速度", "结果"}
	fmt.Println(pad(head[0], 26) + pad(head[1], 32) + pad(head[2], 9) + pad(head[3], 11) + head[4])
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].OK != rows[j].OK {
			return rows[i].OK
		}
		return rows[i].Seconds < rows[j].Seconds
	})
	for _, r := range rows {
		if r.OK {
			fmt.Println(pad(r.Name, 26) + pad(r.Base, 32) + pad(fmt.Sprintf("%.1fs", r.Seconds), 9) +
				pad(xfer.HumanBytes(int64(r.Speed))+"/s", 11) + "✅ 镜像 " + xfer.HumanBytes(r.Bytes))
			continue
		}
		fmt.Println(pad(r.Name, 26) + pad(r.Base, 32) + pad(fmt.Sprintf("%.1fs", r.Seconds), 9) +
			pad("-", 11) + "❌ " + r.Err)
	}
	fmt.Println()
	switch len(digests) {
	case 0:
		fmt.Println("没有任何源可用：内置源也会失败，请用 --mirror 指定你自己的加速地址，或设置 HTTPS_PROXY")
	case 1:
		for d, names := range digests {
			fmt.Printf("内容一致性: %d 个源返回同一 manifest %s ✅\n", len(names), registry.ShortDigest(d))
		}
	default:
		fmt.Println("内容一致性: ⚠️ 不同源返回了不同的 manifest，请只信任下列能互相印证的组合：")
		for _, d := range sortedKeys(digests) {
			fmt.Printf("  %s  <- %s\n", registry.ShortDigest(d), strings.Join(digests[d], ", "))
		}
	}
	fmt.Println("\n用法: dpull pull 镜像 --mirror <上面最快的地址>；不加 --mirror 时官方源不可达会自动按此顺序尝试内置源")
	return countOK(rows), nil
}

// classifyProbeErr turns raw registry noise into something actionable: public
// caches mostly fail because their free tier is rate limited, which is not the
// same problem as being unreachable.
func classifyProbeErr(err error) string {
	msg := trimErr(err)
	low := strings.ToLower(msg)
	if strings.Contains(low, "429") || strings.Contains(low, "toomanyrequests") || strings.Contains(low, "rate exceeded") || strings.Contains(low, "免费") {
		return "速率限制（公开免费额度用满，稍后重试或换源）: " + msg
	}
	return msg
}

// manifestDigest identifies what the tag resolved to, preferring the digest the
// registry reported and falling back to hashing the manifest we received.
func manifestDigest(res *registry.Resolved) string {
	if res.ManifestDigest != "" {
		return res.ManifestDigest
	}
	if res.IndexDigest != "" {
		return res.IndexDigest
	}
	if len(res.ManifestBytes) == 0 {
		return ""
	}
	sum := sha256.Sum256(res.ManifestBytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (o *Options) sourceProbeTimeout() time.Duration {
	if o.Stall > 0 && o.Stall < 15*time.Second {
		return 15 * time.Second
	}
	return 25 * time.Second
}

// readRange pulls the first n bytes of a blob and returns how many arrived.
func readRange(ctx context.Context, c *registry.Client, repo, digest string, n int64) (int64, error) {
	res, err := c.Do(ctx, registry.Request{
		Method:  "GET",
		Repo:    repo,
		Action:  "pull",
		Path:    registry.BlobPath(repo, digest),
		Headers: rangeHeader(n),
		Ok:      func(code int) bool { return code == 200 || code == 206 },
	})
	if err != nil {
		return 0, err
	}
	defer res.Close()
	got, err := io.Copy(io.Discard, io.LimitReader(res.Resp.Body, n))
	return got, err
}

// rangeHeader asks for the first n bytes of a blob.
func rangeHeader(n int64) http.Header {
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	return h
}

// pad right-pads to a display width, counting CJK/full-width runes twice, so
// the table lines up in terminals that render them double-width.
func pad(s string, width int) string {
	w := 0
	for _, r := range s {
		if r > 0x1100 && (r < 0x2000 || r > 0x206F) {
			w += 2
		} else {
			w++
		}
	}
	if w >= width {
		return s + " "
	}
	return s + strings.Repeat(" ", width-w)
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countOK(rows []sourceRow) int {
	n := 0
	for _, r := range rows {
		if r.OK {
			n++
		}
	}
	return n
}
