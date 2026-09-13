package registry

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseProxy(t *testing.T) {
	cases := []struct {
		in      string
		mode    proxyMode
		addr    string
		user    string
		wantErr bool
	}{
		{in: "", mode: proxyNone},
		{in: "direct", mode: proxyDirect},
		{in: "NONE", mode: proxyDirect},
		{in: "http://127.0.0.1:7890", mode: proxyHTTP, addr: "127.0.0.1:7890"},
		{in: "https://proxy.corp:8443", mode: proxyHTTP, addr: "proxy.corp:8443"},
		{in: "socks5://127.0.0.1:1080", mode: proxySOCKS5, addr: "127.0.0.1:1080"},
		{in: "socks5h://127.0.0.1:1080", mode: proxySOCKS5Remote},
		{in: "socks4://127.0.0.1:1080", mode: proxySOCKS4},
		{in: "socks4a://127.0.0.1:1080", mode: proxySOCKS4A},
		{in: "socks5://me:pw@1.2.3.4:1080", mode: proxySOCKS5, addr: "1.2.3.4:1080", user: "me"},
		{in: "127.0.0.1:7890", mode: proxyHTTP, addr: "127.0.0.1:7890"}, // forgiving
		{in: "socks5://127.0.0.1", wantErr: true},                       // no port
		{in: "quic://127.0.0.1:7890", wantErr: true},                    // unsupported
	}
	for _, c := range cases {
		spec, err := parseProxy(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseProxy(%q) expected an error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseProxy(%q): %v", c.in, err)
			continue
		}
		if c.in == "" {
			if spec != nil {
				t.Errorf("parseProxy(\"\") = %+v, want nil", spec)
			}
			continue
		}
		if spec.mode != c.mode {
			t.Errorf("parseProxy(%q).mode = %v want %v", c.in, spec.mode, c.mode)
		}
		if c.addr != "" && spec.addr != c.addr {
			t.Errorf("parseProxy(%q).addr = %q want %q", c.in, spec.addr, c.addr)
		}
		if c.user != "" && spec.user != c.user {
			t.Errorf("parseProxy(%q).user = %q", c.in, spec.user)
		}
	}
	if err := CheckProxy("socks9://x:1"); err == nil {
		t.Error("CheckProxy must reject an unknown protocol")
	}
}

func echoOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "origin-said-hi")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getThrough(t *testing.T, proxy, base string) (string, error) {
	t.Helper()
	return getThroughCfg(t, proxy, base, 15*time.Second)
}

func getThroughCfg(t *testing.T, proxy, base string, timeout time.Duration) (string, error) {
	t.Helper()
	cli := New([]Endpoint{{Name: "t", Base: base, Host: mustHost(t, base)}},
		Config{Concurrency: 2, Proxy: proxy})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := cli.Do(ctx, Request{Method: http.MethodGet, Path: "/"})
	if err != nil {
		return "", err
	}
	defer res.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Resp.Body, 64))
	return string(b), nil
}

// lanOrigin serves on a non-loopback address so the "loopback never goes
// through the proxy" rule cannot mask a proxy test.
func lanOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	var ip string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			ip = n.IP.String()
			break
		}
	}
	if ip == "" {
		t.Skip("本机没有非 loopback 的 IPv4 地址，跳过代理转发测试")
	}
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skip("无法在本机地址上监听:", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "origin-said-hi")
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func mustHost(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// httpproxy is imported for the environment-comparison test.

// startSOCKS5 is a minimal CONNECT-only SOCKS5 server, optionally requiring the
// username/password sub-negotiation.
func startSOCKS5(t *testing.T, user, pass string) (addr string, connects *atomic.Int64) {
	t.Helper()
	n := &atomic.Int64{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				r := bufio.NewReader(c)
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(r, hdr); err != nil || hdr[0] != 0x05 {
					return
				}
				methods := make([]byte, hdr[1])
				if _, err := io.ReadFull(r, methods); err != nil {
					return
				}
				wantAuth := user != ""
				if wantAuth {
					c.Write([]byte{0x05, 0x02})
					am := make([]byte, 1)
					if _, err := io.ReadFull(r, am); err != nil || am[0] != 0x01 {
						return
					}
					readStr := func() string {
						l := make([]byte, 1)
						if _, err := io.ReadFull(r, l); err != nil {
							return ""
						}
						b := make([]byte, l[0])
						io.ReadFull(r, b)
						return string(b)
					}
					u := readStr()
					p := readStr()
					if u != user || p != pass {
						c.Write([]byte{0x01, 0x01})
						return
					}
					c.Write([]byte{0x01, 0x00})
				} else {
					c.Write([]byte{0x05, 0x00})
				}
				req := make([]byte, 4)
				if _, err := io.ReadFull(r, req); err != nil || req[1] != 0x01 {
					return
				}
				var host string
				switch req[3] {
				case 0x01:
					b := make([]byte, 4)
					io.ReadFull(r, b)
					host = net.IP(b).String()
				case 0x03:
					l := make([]byte, 1)
					io.ReadFull(r, l)
					b := make([]byte, l[0])
					io.ReadFull(r, b)
					host = string(b)
				case 0x04:
					b := make([]byte, 16)
					io.ReadFull(r, b)
					host = "[" + net.IP(b).String() + "]"
				default:
					c.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0})
					return
				}
				pb := make([]byte, 2)
				io.ReadFull(r, pb)
				port := int(pb[0])<<8 | int(pb[1])
				up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
				if err != nil {
					c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				n.Add(1)
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, pb[0], pb[1]})
				go io.Copy(up, r)
				io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().String(), n
}

func TestSOCKS5Proxy(t *testing.T) {
	origin := echoOrigin(t)
	addr, connects := startSOCKS5(t, "", "")
	body, err := getThrough(t, "socks5://"+addr, origin.URL)
	if err != nil {
		t.Fatalf("request through SOCKS5 failed: %v", err)
	}
	if body != "origin-said-hi" {
		t.Fatalf("body = %q", body)
	}
	if connects.Load() == 0 {
		t.Error("the SOCKS5 server never saw a CONNECT")
	}
}

func TestSOCKS5RemoteResolution(t *testing.T) {
	origin := echoOrigin(t)
	addr, connects := startSOCKS5(t, "", "")
	if _, err := getThrough(t, "socks5h://"+addr, origin.URL); err != nil {
		t.Fatalf("socks5h failed: %v", err)
	}
	if connects.Load() == 0 {
		t.Error("the proxy never connected to the target")
	}
}

func TestSOCKS5Auth(t *testing.T) {
	origin := echoOrigin(t)
	addr, _ := startSOCKS5(t, "me", "s3cret")
	if _, err := getThrough(t, "socks5://me:s3cret@"+addr, origin.URL); err != nil {
		t.Fatalf("authenticated SOCKS5 failed: %v", err)
	}
	_, err := getThrough(t, "socks5://me:wrong@"+addr, origin.URL)
	if err == nil {
		t.Fatal("wrong credentials must fail")
	}
	if !strings.Contains(err.Error(), "认证") {
		t.Errorf("want a clear auth error, got %v", err)
	}
	_, err = getThrough(t, "socks5://"+addr, origin.URL)
	if err == nil || !strings.Contains(err.Error(), "要求认证") {
		t.Errorf("missing credentials must be explained, got %v", err)
	}
}

// startSOCKS4A answers SOCKS4a requests, resolving the domain itself.
func startSOCKS4A(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	n := &atomic.Int64{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				r := bufio.NewReader(c)
				hdr := make([]byte, 8)
				if _, err := io.ReadFull(r, hdr); err != nil || hdr[0] != 0x04 {
					return
				}
				port := int(hdr[2])<<8 | int(hdr[3])
				// skip the NUL-terminated user id
				for {
					b, err := r.ReadByte()
					if err != nil || b == 0 {
						break
					}
				}
				host := ""
				if hdr[4] == 0 && hdr[5] == 0 && hdr[6] == 0 && hdr[7] != 0 {
					// SOCKS4a: the domain follows
					h, err := r.ReadString(0)
					if err != nil {
						return
					}
					host = trimNUL(h)
				} else {
					host = net.IP(hdr[4:8]).String()
				}
				up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
				if err != nil {
					c.Write([]byte{0x00, 0x5B, hdr[2], hdr[3], 0, 0, 0, 0})
					return
				}
				defer up.Close()
				n.Add(1)
				c.Write([]byte{0x00, 0x5A, hdr[2], hdr[3], 0, 0, 0, 0})
				go io.Copy(up, r)
				io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().String(), n
}

func trimNUL(s string) string {
	for len(s) > 0 && s[len(s)-1] == 0 {
		s = s[:len(s)-1]
	}
	return s
}

func TestSOCKS4AProxy(t *testing.T) {
	origin := echoOrigin(t)
	addr, connects := startSOCKS4A(t)
	body, err := getThrough(t, "socks4a://"+addr, origin.URL)
	if err != nil {
		t.Fatalf("request through SOCKS4a failed: %v", err)
	}
	if body != "origin-said-hi" {
		t.Fatalf("body = %q", body)
	}
	if connects.Load() == 0 {
		t.Error("the SOCKS4a server never connected to the target")
	}
}

// startHTTPProxy is a forwarding proxy: it accepts absolute-URI GETs (which is
// what net/http sends for a plain http target) and replays them.
func startHTTPProxy(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	n := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Scheme != "" && r.URL.Host != "" {
			req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			defer res.Body.Close()
			for k, vv := range res.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(res.StatusCode)
			io.Copy(w, res.Body)
			return
		}
		http.Error(w, "proxy saw a non-absolute URI: "+r.RequestURI, 400)
	}))
	t.Cleanup(srv.Close)
	return strings_TrimPrefix(srv.URL, "http://"), n
}

func strings_TrimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

func TestHTTPProxy(t *testing.T) {
	origin := lanOrigin(t)
	addr, hits := startHTTPProxy(t)
	body, err := getThrough(t, "http://"+addr, origin.URL)
	if err != nil {
		t.Fatalf("request through the http proxy failed: %v", err)
	}
	if body != "origin-said-hi" {
		t.Fatalf("body = %q", body)
	}
	if hits.Load() == 0 {
		t.Error("the http proxy was never used")
	}
}

func TestDirectClearsTransportProxy(t *testing.T) {
	lan := "http://192.0.2.10:5000/v2/"
	d := newIPPrefDial(Config{})

	// no --proxy: the environment-based resolver stays in charge
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if err := applyProxy(tr, d, nil); err != nil {
		t.Fatal(err)
	}
	if tr.Proxy == nil {
		t.Error("an empty --proxy must leave HTTP_PROXY handling alone")
	}

	// --proxy direct: nothing gets proxied, stale HTTP_PROXY included
	tr2 := &http.Transport{Proxy: http.ProxyFromEnvironment}
	spec, err := parseProxy("direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyProxy(tr2, d, spec); err != nil {
		t.Fatal(err)
	}
	if tr2.Proxy != nil {
		u2, err := tr2.Proxy(&http.Request{URL: mustParse(t, lan)})
		if err != nil || u2 != nil {
			t.Errorf("--proxy direct must disable proxying, got %v %v", u2, err)
		}
	}

	// explicit --proxy applies to remote hosts but never to a local registry,
	// which is the trap when someone exports a proxy globally
	tr3 := &http.Transport{}
	spec3, err := parseProxy("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyProxy(tr3, d, spec3); err != nil {
		t.Fatal(err)
	}
	if u3, _ := tr3.Proxy(&http.Request{URL: mustParse(t, lan)}); u3 == nil || u3.Port() != "7890" {
		t.Errorf("remote target should use the proxy, got %v", u3)
	}
	for _, local := range []string{"http://localhost:5000/v2/", "http://127.0.0.1:5000/v2/"} {
		if u3, _ := tr3.Proxy(&http.Request{URL: mustParse(t, local)}); u3 != nil {
			t.Errorf("%s must bypass the proxy, got %v", local, u3)
		}
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLoopbackTargetSkipsProxy(t *testing.T) {
	origin := echoOrigin(t) // 127.0.0.1
	addr, hits := startHTTPProxy(t)
	if _, err := getThrough(t, "http://"+addr, origin.URL); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("a loopback registry must not go through the proxy (%d hits)", hits.Load())
	}
}

func TestWrongProtocolGivesActionableError(t *testing.T) {
	origin := lanOrigin(t)
	proxyAddr, _ := startHTTPProxy(t)
	var notesMu sync.Mutex
	var notes []string
	cli := New([]Endpoint{{Name: "t", Base: origin.URL, Host: mustHost(t, origin.URL)}}, Config{
		Concurrency: 2,
		Proxy:       "socks5://" + proxyAddr,
		Note: func(f string, args ...any) {
			notesMu.Lock()
			defer notesMu.Unlock()
			notes = append(notes, fmt.Sprintf(f, args...))
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if _, err := cli.Do(ctx, Request{Method: http.MethodGet, Path: "/"}); err == nil {
		t.Fatal("pointing socks5:// at an http proxy must fail")
	}
	notesMu.Lock()
	joined := strings.Join(notes, "|")
	notesMu.Unlock()
	if !strings.Contains(joined, "HTTP 代理") && !strings.Contains(joined, "协议") {
		t.Errorf("want an actionable protocol hint, got %q", joined)
	}
}

func TestUnreachableProxyReportsProxy(t *testing.T) {
	origin := echoOrigin(t)
	_, err := getThrough(t, "socks5://127.0.0.1:1", origin.URL)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "代理") {
		t.Errorf("the error must name the proxy, got %v", err)
	}
}

func TestBadProxyValueFailsRequests(t *testing.T) {
	cli := New([]Endpoint{{Name: "t", Base: "http://127.0.0.1:1", Host: "127.0.0.1:1"}}, Config{Proxy: "quic://x:1"})
	if _, err := cli.Do(context.Background(), Request{Method: http.MethodGet, Path: "/"}); err == nil {
		t.Fatal("a rejected --proxy value must fail every request")
	}
}
