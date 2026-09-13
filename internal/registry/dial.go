package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config tunes the transport used for every registry request.
type Config struct {
	Concurrency   int
	SkipTLSVerify bool
	// PreferIPv4 dials A records first: poisoned AAAA answers that point at
	// unreachable IPv6 addresses are common on mainland-China networks.
	PreferIPv4 bool
	// DohServers re-resolve a hostname when the system resolver's answers all
	// fail to connect. UDP/53 is transparently hijacked on many networks there,
	// so the system answer can be a blackhole IP while the host is fine.
	// Pass DisableDoH to opt out, or DohServers to change the list.
	DohServers    []string
	DisableDoH    bool
	HostOverrides []string // "host=ip", like curl --resolve
	Verbose       bool
	// Note receives user-facing diagnostics such as "poisoned DNS bypassed".
	Note func(format string, args ...any)
}

// DefaultDoHServers are JSON DNS APIs that are typically reachable and answer
// without poisoning. They are tried in order; the first valid answer wins.
var DefaultDoHServers = []string{
	"https://doh.pub/dns-query",
	"https://dns.alidns.com/resolve",
	"https://cloudflare-dns.com/dns-query",
	"https://dns.google/resolve",
}

// ipPrefDial resolves a name itself, dials the preferred family first with a
// per-address timeout, and falls back to DNS-over-HTTPS when every address the
// system resolver handed us is unreachable.
type ipPrefDial struct {
	dialer     *net.Dialer
	preferIPv4 bool
	verbose    bool
	overrides  hostOverrides
	doh        *dohResolver
	note       func(format string, args ...any)
	noted      map[string]bool

	mu     sync.Mutex
	ttl    map[string][]net.IPAddr
	stamps map[string]time.Time
}

func newIPPrefDial(cfg Config) *ipPrefDial {
	d := &ipPrefDial{
		dialer: &net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 30 * time.Second,
		},
		preferIPv4: cfg.PreferIPv4,
		verbose:    cfg.Verbose,
		overrides:  parseHostOverrides(cfg.HostOverrides),
		note:       cfg.Note,
		noted:      map[string]bool{},
		ttl:        map[string][]net.IPAddr{},
		stamps:     map[string]time.Time{},
	}
	if !cfg.DisableDoH {
		d.doh = newDoHResolver(cfg.DohServers)
	}
	return d
}

// dnsCacheTTL keeps retries cheap without pinning addresses for too long.
const dnsCacheTTL = 30 * time.Second

// perAddressTimeout bounds one connect attempt; a dead address must not eat
// the whole budget of a chunk download.
const perAddressTimeout = 8 * time.Second

func (d *ipPrefDial) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if addr == "" {
		return nil, fmt.Errorf("empty address")
	}
	if mapped, ok := d.overrides.resolve(addr); ok {
		if d.verbose {
			debug("override %s -> %s", addr, mapped)
		}
		return d.dialer.DialContext(ctx, network, mapped)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.dialer.DialContext(ctx, network, addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		return d.dialer.DialContext(ctx, network, addr)
	}

	ips, sysErr := d.lookup(ctx, host)
	conn, firstErr := d.dialAll(ctx, network, port, ips)
	if conn != nil {
		return conn, nil
	}
	// Every system answer failed. On a hijacked network that is the signature
	// of a poisoned record, so ask an encrypted resolver before giving up.
	if d.doh != nil {
		if clean, derr := d.doh.lookup(ctx, host, d.preferIPv4); derr == nil && len(clean) > 0 {
			if conn2, _ := d.dialAll(ctx, network, port, clean); conn2 != nil {
				d.cacheIPs(host, clean)
				d.noteOnce(host, "%s 的系统解析结果不可用（%s），DoH 重新解析到 %s，已绕过被污染的 DNS",
					host, whyUnreachable(sysErr, firstErr), briefAddrs(clean))
				return conn2, nil
			}
			d.noteOnce(host, "%s 已用 DoH 解析到真实地址 %s，但连接仍被中断——这不是 DNS 污染，是 SNI/IP 层干扰，需要 --mirror 加速地址或 HTTPS_PROXY 代理",
				host, briefAddrs(clean))
			if d.verbose {
				debug("DoH 解析 %s -> %s，仍然连不上", host, briefAddrs(clean))
			}
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no usable address for %s", host)
	}
	return nil, firstErr
}

// dialAll tries each address in order with its own deadline.
func (d *ipPrefDial) dialAll(ctx context.Context, network, port string, ips []net.IPAddr) (net.Conn, error) {
	var lastErr error
	for _, ip := range ips {
		cctx, cancel := context.WithTimeout(ctx, perAddressTimeout)
		conn, err := d.dialer.DialContext(cctx, network, net.JoinHostPort(ip.String(), port))
		cancel()
		if err == nil {
			if d.verbose {
				debug("dial %s -> %s", ip.String(), port)
			}
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (d *ipPrefDial) lookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	d.mu.Lock()
	if ips, ok := d.ttl[host]; ok && time.Since(d.stamps[host]) < dnsCacheTTL {
		d.mu.Unlock()
		return ips, nil
	}
	d.mu.Unlock()

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips = d.order(ips)
	d.cacheIPs(host, ips)
	return ips, nil
}

func (d *ipPrefDial) cacheIPs(host string, ips []net.IPAddr) {
	d.mu.Lock()
	d.ttl[host] = ips
	d.stamps[host] = time.Now()
	d.mu.Unlock()
}

// order sorts by the preferred family and rotates the first two entries so one
// broken address is not always tried first.
func (d *ipPrefDial) order(ips []net.IPAddr) []net.IPAddr {
	if d.preferIPv4 {
		sort.SliceStable(ips, func(i, j int) bool {
			return ips[i].IP.To4() != nil && ips[j].IP.To4() == nil
		})
	} else {
		sort.SliceStable(ips, func(i, j int) bool {
			return ips[i].IP.To4() == nil && ips[j].IP.To4() != nil
		})
	}
	if len(ips) > 1 {
		rand.Shuffle(2, func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
	}
	return ips
}

// noteOnce reports a diagnostic for a host at most once per run.
func (d *ipPrefDial) noteOnce(host, format string, args ...any) {
	d.mu.Lock()
	if d.noted[host] || d.note == nil {
		d.mu.Unlock()
		return
	}
	d.noted[host] = true
	note := d.note
	d.mu.Unlock()
	note(format, args...)
}

// briefAddrs renders at most two addresses plus a count.
func briefAddrs(ips []net.IPAddr) string {
	if len(ips) == 0 {
		return "-"
	}
	if len(ips) == 1 {
		return ips[0].IP.String()
	}
	return fmt.Sprintf("%s 等 %d 个地址", ips[0].IP, len(ips))
}

// whyUnreachable explains whether the system resolver lied or the network died.
func whyUnreachable(sysErr, dialErr error) string {
	if sysErr != nil {
		return "解析失败"
	}
	return trimErr(dialErr)
}

// dohResolver is a tiny JSON DNS client (RFC 8484 companion API) that speaks to
// Cloudflare/Google/dnspod/alidns style endpoints. It uses its own transport
// with a plain dialer so resolution can never recurse into itself.
type dohResolver struct {
	servers []string
	client  *http.Client
	ttl     time.Duration

	mu     sync.Mutex
	cache  map[string][]net.IPAddr
	stamps map[string]time.Time
}

func newDoHResolver(servers []string) *dohResolver {
	if len(servers) == 0 {
		servers = DefaultDoHServers
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 6 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   2,
		TLSHandshakeTimeout:   6 * time.Second,
		ResponseHeaderTimeout: 6 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &dohResolver{
		servers: servers,
		client:  &http.Client{Transport: tr, Timeout: 10 * time.Second},
		ttl:     5 * time.Minute,
		cache:   map[string][]net.IPAddr{},
		stamps:  map[string]time.Time{},
	}
}

type dohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	Data string `json:"data"`
}

type dohResponse struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

// lookup resolves A and AAAA records over HTTPS and orders them by preference.
func (r *dohResolver) lookup(ctx context.Context, host string, preferIPv4 bool) ([]net.IPAddr, error) {
	key := host
	r.mu.Lock()
	if ips, ok := r.cache[key]; ok && time.Since(r.stamps[key]) < r.ttl {
		r.mu.Unlock()
		return ips, nil
	}
	r.mu.Unlock()

	families := []struct {
		qtype string
		want  int
	}{
		{"A", 1},
		{"AAAA", 28},
	}
	if !preferIPv4 {
		families = []struct {
			qtype string
			want  int
		}{
			{"AAAA", 28},
			{"A", 1},
		}
	}
	var out []net.IPAddr
	var lastErr error
	for _, f := range families {
		for _, server := range r.servers {
			resp, err := r.query(ctx, server, host, f.qtype)
			if err != nil {
				lastErr = err
				continue
			}
			if resp.Status != 0 {
				lastErr = fmt.Errorf("%s 返回 Status=%d", server, resp.Status)
				continue
			}
			found := 0
			for _, a := range resp.Answer {
				if a.Type != f.want {
					continue
				}
				if ip := net.ParseIP(strings.TrimSuffix(a.Data, ".")); ip != nil {
					out = append(out, net.IPAddr{IP: ip})
					found++
				}
			}
			if found > 0 {
				lastErr = nil
				break
			}
		}
	}
	if len(out) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("DoH 对 %s 没有可用记录", host)
		}
		return nil, lastErr
	}
	r.mu.Lock()
	r.cache[key] = out
	r.stamps[key] = time.Now()
	r.mu.Unlock()
	return out, nil
}

func (r *dohResolver) query(ctx context.Context, server, host, qtype string) (*dohResponse, error) {
	u := server
	sep := "?"
	if strings.Contains(server, "?") {
		sep = "&"
	}
	u += sep + "name=" + host + "&type=" + qtype
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// the JSON API is negotiated this way; servers that only speak wire format
	// (RFC 8484) simply answer in the same JSON shape for these endpoints
	req.Header.Set("Accept", "application/dns-json")
	res, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: HTTP %d", server, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out dohResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s: 响应不是 dns-json: %w", server, err)
	}
	return &out, nil
}

// hostOverrides maps a hostname to a fixed IP address.
type hostOverrides map[string]string

func parseHostOverrides(in []string) hostOverrides {
	out := hostOverrides{}
	for _, item := range in {
		host, ip, ok := strings.Cut(strings.TrimSpace(item), "=")
		host = strings.TrimSpace(host)
		ip = strings.Trim(strings.TrimSpace(ip), "[]")
		if !ok || host == "" || ip == "" {
			continue
		}
		out[host] = ip
	}
	return out
}

func (o hostOverrides) resolve(addr string) (string, bool) {
	if len(o) == 0 {
		return addr, false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, false
	}
	if ip, ok := o[host]; ok {
		return net.JoinHostPort(ip, port), true
	}
	return addr, false
}
