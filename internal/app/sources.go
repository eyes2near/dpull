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
	Note string
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

// fallbackSources is ordered by measured throughput from a mainland-China
// network on 2026-09-13 (redis:7.2-alpine, 15.6MiB, 16 connections):
// 1panel 5s, daocloud 5s, ecr 6s, xuanyuan 16s. docker.1ms.run served the same
// image in 3m46s and is deliberately left out. Public caches come and go, so
// `dpull sources` re-measures them and the run itself always falls through to
// the next candidate on failure.
var fallbackSources = []fallbackSource{
	{Name: "1panel", Base: "https://docker.1panel.live", Repo: hubMirror, Note: "公开 Docker Hub 缓存"},
	{Name: "daocloud", Base: "https://docker.m.daocloud.io", Repo: hubMirror, Note: "DaoCloud 公开加速"},
	{Name: "ecr", Base: "https://public.ecr.aws", Repo: ecrAlias, Note: "AWS 官方 Docker Hub 透传副本"},
	{Name: "xuanyuan", Base: "https://docker.xuanyuan.me", Repo: hubMirror, Note: "公开 Docker Hub 缓存"},
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
	name = strings.TrimPrefix(name, "内置:")
	switch name {
	case "ecr":
		return "内置源 AWS ECR 官方透传副本"
	default:
		return "内置源 " + name
	}
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
	try := func(name, base, repo string) {
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

	try("origin:"+ref.Host(), ref.Host(), ref.Repository)
	for i, m := range o.Mirrors {
		try(fmt.Sprintf("mirror%d", i+1), m, ref.Repository)
	}
	for _, s := range dockerDaemonMirrors() {
		try("daemon-mirror", s, ref.Repository)
	}
	for _, s := range fallbackSources {
		repo, ok := s.Repo(ref)
		if !ok {
			continue
		}
		try("内置:"+s.Name, s.Base, repo)
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
