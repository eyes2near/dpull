package registry

// Outbound proxy support.
//
// --proxy accepts http://, https://, socks5://, socks5h://, socks4:// and
// socks4a:// (optionally with user:pass). http/https proxies are handled by
// net/http itself, which issues CONNECT for TLS targets; the SOCKS variants are
// implemented here because the standard library has no SOCKS client that
// net/http will use for arbitrary transports, and none for SOCKS4 at all.
//
// Why SOCKS matters on top of the DNS work: a proxy is the one thing that
// defeats SNI-level interference, because the target hostname never appears in
// a ClientHello that the network can read.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrProxy marks a failure caused by the proxy itself rather than by the
// registry behind it. Callers use it to stop walking sources: when the local
// proxy is down, the fourth source will not do any better than the first.
var ErrProxy = errors.New("代理不可用")

// proxyDialError wraps a proxy connect failure with ErrProxy.
func proxyDialError(mode proxyMode, addr string, err error) error {
	return fmt.Errorf("%w: 连接 %s 代理 %s 失败: %w", ErrProxy, mode, addr, err)
}

// proxyMode is what a parsed --proxy value asks for.
type proxyMode int

const (
	proxyNone         proxyMode = iota // nothing configured: follow the environment
	proxyDirect                        // explicitly bypass any environment proxy
	proxyHTTP                          // http/https CONNECT proxy, handled by net/http
	proxySOCKS5                        // SOCKS5, target resolved locally
	proxySOCKS5Remote                  // SOCKS5h, target resolved by the proxy
	proxySOCKS4                        // SOCKS4, requires an IPv4 target
	proxySOCKS4A                       // SOCKS4a, target resolved by the proxy
)

func (m proxyMode) String() string {
	switch m {
	case proxyDirect:
		return "direct"
	case proxyHTTP:
		return "http"
	case proxySOCKS5:
		return "socks5"
	case proxySOCKS5Remote:
		return "socks5h"
	case proxySOCKS4:
		return "socks4"
	case proxySOCKS4A:
		return "socks4a"
	default:
		return "none"
	}
}

// proxySpec is a parsed proxy address.
type proxySpec struct {
	mode proxyMode
	addr string // host:port of the proxy server
	user string
	pass string
	orig string
}

func (p *proxySpec) String() string {
	if p == nil {
		return ""
	}
	return p.orig
}

// schemeOf extracts the scheme of a proxy URL for display.
func schemeOf(raw string) string {
	if i := strings.Index(raw, "://"); i > 0 {
		return raw[:i]
	}
	return ""
}

// parseProxy normalises a --proxy value. Empty means "honour HTTP_PROXY and
// friends"; "direct"/"none"/"off" forces a clean bypass of them.
func parseProxy(raw string) (*proxySpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	switch strings.ToLower(raw) {
	case "direct", "none", "off", "no", "-":
		return &proxySpec{mode: proxyDirect, orig: raw}, nil
	}
	scheme := schemeOf(raw)
	if scheme == "" {
		// be forgiving: "127.0.0.1:7890" is what people type first
		raw = "http://" + raw
		scheme = "http"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无法解析代理地址 %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理地址 %q 缺少主机", raw)
	}
	spec := &proxySpec{addr: u.Host, orig: raw}
	if u.User != nil {
		spec.user = u.User.Username()
		spec.pass, _ = u.User.Password()
	}
	switch strings.ToLower(scheme) {
	case "http", "https":
		spec.mode = proxyHTTP
	case "socks5":
		spec.mode = proxySOCKS5
	case "socks5h":
		spec.mode = proxySOCKS5Remote
	case "socks4":
		spec.mode = proxySOCKS4
	case "socks4a":
		spec.mode = proxySOCKS4A
	case "socks":
		spec.mode = proxySOCKS5
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q（可用: http, https, socks5, socks5h, socks4, socks4a, direct）", scheme)
	}
	if _, _, err := net.SplitHostPort(spec.addr); err != nil {
		return nil, fmt.Errorf("代理地址 %q 需要带端口，如 %s://127.0.0.1:7890", u.Host, scheme)
	}
	if (spec.mode == proxySOCKS4 || spec.mode == proxySOCKS4A) && (spec.user == "" && spec.pass == "") {
		// SOCKS4 has no auth handshake but does carry a user id; some servers
		// reject an empty one, so send the local username when we have it.
		spec.user = localUser()
	}
	return spec, nil
}

// localUser fills the SOCKS4 user-id field, which some servers insist on.
func localUser() string {
	for _, k := range []string{"USER", "LOGNAME", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "dpull"
}

// applyProxy rewires a transport to use the given proxy. The dialer is reused to
// reach the proxy itself, so a poisoned proxy hostname still gets the DoH
// treatment.
func applyProxy(tr *http.Transport, d *ipPrefDial, spec *proxySpec) error {
	if spec == nil {
		return nil // leave http.ProxyFromEnvironment in place
	}
	switch spec.mode {
	case proxyDirect:
		tr.Proxy = nil
		return nil
	case proxyHTTP:
		u, err := url.Parse(spec.orig)
		if err != nil {
			return err
		}
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			if isLocalTarget(req) {
				return nil, nil
			}
			return u, nil
		}
		return nil
	default:
		tr.Proxy = nil
		sd := &socksDialer{spec: spec, via: d}
		tr.DialContext = sd.DialContext
		return nil
	}
}

// isLocalTarget keeps a loopback registry off the proxy: pointing dpull at a
// local registry while a corporate proxy is configured is a normal thing to do.
func isLocalTarget(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	h := req.URL.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// socksDialer turns "dial the final registry" into "dial the proxy, then ask it
// to connect". Every parallel chunk gets its own tunnel, which is exactly what
// the downloader wants.
type socksDialer struct {
	spec *proxySpec
	via  *ipPrefDial
}

func (s *socksDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS 代理只能转发 TCP，不支持 %q", network)
	}
	var (
		conn net.Conn
		err  error
	)
	if s.via != nil {
		conn, err = s.via.DialContext(ctx, "tcp", s.spec.addr)
	} else {
		d := &net.Dialer{Timeout: 20 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", s.spec.addr)
	}
	if err != nil {
		return nil, proxyDialError(s.spec.mode, s.spec.addr, err)
	}
	if err := socksHandshake(ctx, conn, s.spec, addr, s.via); err != nil {
		_ = conn.Close()
		// net/http silently retries a failed dial until the request deadline, so
		// surface the reason now or the user just sees "context deadline exceeded".
		if s.via != nil {
			s.via.noteAlways("socks:"+s.spec.addr, "%s 代理握手失败：%s", s.spec.mode, err)
		}
		return nil, err
	}
	return conn, nil
}

// socksHandshakeTimeout bounds the negotiation: a peer that stays silent is
// almost always an HTTP proxy on a port someone typed as socks5://.
const socksHandshakeTimeout = 5 * time.Second

// SOCKS protocol constants.
const (
	socks5Version      = 0x05
	socks5AuthNone     = 0x00
	socks5AuthPasswd   = 0x02
	socks5AuthNoAccept = 0xFF
	socks5CmdConnect   = 0x01
	socks5AtypIPv4     = 0x01
	socks5AtypDomain   = 0x03
	socks5AtypIPv6     = 0x04
	socks5OK           = 0x00
	socks4Version      = 0x04
	// SOCKS4 reply codes are decimal 90..93, a classic off-by-radix trap.
	socks4OK        = 90
	socks4Rejected  = 91
	socks4Identd    = 92
	socks4IdentUser = 93
)

var socks5Errors = map[byte]string{
	0x01: "general SOCKS server failure",
	0x02: "connection not allowed by ruleset",
	0x03: "network unreachable",
	0x04: "host unreachable",
	0x05: "connection refused",
	0x06: "TTL expired",
	0x07: "command not supported",
	0x08: "address type not supported",
}

// socksHandshake performs the client side of SOCKS4/4a/5 and leaves conn ready
// to carry the TLS handshake to the target.
func socksHandshake(ctx context.Context, conn net.Conn, spec *proxySpec, target string, via *ipPrefDial) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("代理目标地址 %q 无效: %w", target, err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("代理目标端口无效: %q", portStr)
	}
	// Bound the negotiation on our own clock: a peer that accepts the socket and
	// then stays silent is nearly always an HTTP proxy on a port someone typed
	// as socks5://, and waiting for the request deadline only hides that.
	deadline := time.Now().Add(socksHandshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})
	switch spec.mode {
	case proxySOCKS5, proxySOCKS5Remote:
		return socks5Connect(conn, spec, host, uint16(port), via, ctx)
	case proxySOCKS4, proxySOCKS4A:
		return socks4Connect(conn, spec, host, uint16(port), via, ctx)
	}
	return fmt.Errorf("内部错误: 不是 SOCKS 代理")
}

func socks5Connect(conn net.Conn, spec *proxySpec, host string, port uint16, via *ipPrefDial, ctx context.Context) error {
	methods := []byte{socks5AuthNone}
	if spec.user != "" {
		methods = append(methods, socks5AuthPasswd)
	}
	hello := make([]byte, 0, 3+len(methods))
	hello = append(hello, socks5Version, byte(len(methods)))
	hello = append(hello, methods...)
	if _, err := conn.Write(hello); err != nil {
		return fmt.Errorf("SOCKS5 握手发送失败: %w", err)
	}
	b, err := readN(conn, 2)
	if err != nil {
		return socksVersionErr(err, conn)
	}
	if b[0] != socks5Version {
		return fmt.Errorf("对端不是 SOCKS5 代理（返回版本 0x%02x，检查一下 --proxy 协议）", b[0])
	}
	switch b[1] {
	case socks5AuthNoAccept:
		return errors.New("SOCKS5 代理拒绝所有认证方式（检查 --proxy 里的用户名密码）")
	case socks5AuthPasswd:
		if spec.user == "" {
			return errors.New("SOCKS5 代理要求认证，请在 --proxy 里写 socks5://user:pass@host:port")
		}
		auth := []byte{0x01, byte(len(spec.user))}
		auth = append(auth, spec.user...)
		auth = append(auth, byte(len(spec.pass)))
		auth = append(auth, spec.pass...)
		if _, err := conn.Write(auth); err != nil {
			return fmt.Errorf("SOCKS5 认证发送失败: %w", err)
		}
		ab, err := readN(conn, 2)
		if err != nil {
			return err
		}
		if ab[1] != 0x00 {
			return errors.New("SOCKS5 认证被拒绝（用户名或密码不对）")
		}
	case socks5AuthNone:
	default:
		return fmt.Errorf("SOCKS5 代理选择了未知认证方式 0x%02x", b[1])
	}

	req := []byte{socks5Version, socks5CmdConnect, 0x00}
	atyp, addrBytes, err := socks5Target(spec, host, via, ctx)
	if err != nil {
		return err
	}
	req = append(req, atyp)
	req = append(req, addrBytes...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 发送失败: %w", err)
	}
	hdr, err := readN(conn, 4)
	if err != nil {
		return err
	}
	if hdr[1] != socks5OK {
		msg := socks5Errors[hdr[1]]
		if msg == "" {
			msg = fmt.Sprintf("code 0x%02x", hdr[1])
		}
		return fmt.Errorf("SOCKS5 代理无法连接目标: %s", msg)
	}
	// drain the bound address so the tunnel is byte-aligned
	switch hdr[3] {
	case socks5AtypIPv4:
		_, err = readN(conn, 4+2)
	case socks5AtypIPv6:
		_, err = readN(conn, 16+2)
	case socks5AtypDomain:
		var n [1]byte
		if _, err = io.ReadFull(conn, n[:]); err == nil {
			_, err = readN(conn, int(n[0])+2)
		}
	default:
		err = fmt.Errorf("SOCKS5 代理返回未知地址类型 0x%02x", hdr[3])
	}
	return err
}

// socks5Target decides whether the proxy or we resolve the hostname.
func socks5Target(spec *proxySpec, host string, via *ipPrefDial, ctx context.Context) (byte, []byte, error) {
	if spec.mode == proxySOCKS5Remote {
		if len(host) > 255 {
			return 0, nil, fmt.Errorf("域名过长: %s", host)
		}
		return socks5AtypDomain, append([]byte{byte(len(host))}, host...), nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return encodeSocksIP(ip)
	}
	// local resolution: reuse the resolver chain so a poisoned answer gets the
	// DoH fallback here too
	ips, err := resolveVia(ctx, via, host)
	if err != nil {
		return 0, nil, fmt.Errorf("SOCKS5 需要本地解析 %s 失败: %w（可改用 socks5h:// 让代理端解析）", host, err)
	}
	return encodeSocksIP(ips[0])
}

func encodeSocksIP(ip net.IP) (byte, []byte, error) {
	if v4 := ip.To4(); v4 != nil {
		return socks5AtypIPv4, []byte(v4), nil
	}
	if v16 := ip.To16(); v16 != nil {
		return socks5AtypIPv6, []byte(v16), nil
	}
	return 0, nil, fmt.Errorf("无法编码地址 %s", ip)
}

// socks4Connect speaks SOCKS4 (connect by IPv4) and SOCKS4a (proxy resolves the
// hostname, encoded as a reserved 0.0.0.x address).
func socks4Connect(conn net.Conn, spec *proxySpec, host string, port uint16, via *ipPrefDial, ctx context.Context) error {
	user := spec.user
	req := []byte{socks4Version, socks5CmdConnect}
	req = binary.BigEndian.AppendUint16(req, port)
	ip := net.ParseIP(host)
	if ip == nil && spec.mode == proxySOCKS4 {
		ips, err := resolveVia(ctx, via, host)
		if err != nil {
			return fmt.Errorf("SOCKS4 需要 IPv4 地址，解析 %s 失败: %w（可改用 socks4a:// 让代理端解析）", host, err)
		}
		ip = ips[0]
	}
	switch {
	case ip != nil && ip.To4() != nil:
		req = append(req, ip.To4()...)
	case spec.mode == proxySOCKS4A:
		// 0.0.0.x in the address field means "proxy, you resolve the name"
		req = append(req, 0, 0, 0, 1)
	case ip != nil:
		return fmt.Errorf("SOCKS4 不支持 IPv6 目标 %s（请用 socks5:// 或 socks4a://）", host)
	default:
		return fmt.Errorf("SOCKS4a 内部错误: 缺少目标地址")
	}
	req = append(req, user...)
	req = append(req, 0)
	if spec.mode == proxySOCKS4A && ip == nil {
		// the domain follows the user-id for 4a
		if len(host) > 255 {
			return fmt.Errorf("域名过长: %s", host)
		}
		req = append(req, host...)
		req = append(req, 0)
	}
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS4 请求发送失败: %w", err)
	}
	resp, err := readN(conn, 8)
	if err != nil {
		return socksVersionErr(err, conn)
	}
	if resp[0] != 0x00 {
		return fmt.Errorf("对端不是 SOCKS4 代理（返回版本 0x%02x，检查一下 --proxy 协议）", resp[0])
	}
	if resp[1] != socks4OK {
		switch resp[1] {
		case socks4Rejected:
			return errors.New("SOCKS4 代理拒绝连接目标")
		case socks4Identd, socks4IdentUser:
			return errors.New("SOCKS4 代理要求的 identd 认证不可用（请用 socks4a:// 或 socks5://）")
		default:
			return fmt.Errorf("SOCKS4 代理拒绝连接: code 0x%02x", resp[1])
		}
	}
	return nil
}

// resolveVia resolves through the dialer's chain (system then DoH) when one is
// available, otherwise with the default resolver.
func resolveVia(ctx context.Context, via *ipPrefDial, host string) ([]net.IP, error) {
	if via != nil {
		if ips, err := via.lookup(ctx, host); err == nil && len(ips) > 0 {
			out := make([]net.IP, 0, len(ips))
			for _, ip := range ips {
				out = append(out, ip.IP)
			}
			if prefer, ok := via.pickIPv4(out); ok {
				return []net.IP{prefer}, nil
			}
			return out, nil
		}
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("没有解析结果")
	}
	return ips, nil
}

func readN(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// socksVersionErr turns "the other side answered HTTP" into an actionable hint,
// which is the usual result of pointing socks5:// at an http proxy port.
func socksVersionErr(err error, conn net.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	probe, _ := readN(conn, 5)
	_ = conn.SetReadDeadline(time.Time{})
	if len(probe) > 0 && (probe[0] == 'H' || probe[0] == 'h') {
		return errors.New("对端像是 HTTP 代理而不是 SOCKS 代理，请把 --proxy 改成 http://")
	}
	if errors.Is(err, io.EOF) {
		return errors.New("代理在握手阶段直接断开连接（协议或端口不对）")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return errors.New("代理没有按 SOCKS 协议回应（端口上多半是 HTTP 代理，请改用 --proxy http://…）")
	}
	return fmt.Errorf("SOCKS 握手失败: %w", err)
}
