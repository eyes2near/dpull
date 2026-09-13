// Package registry implements a small Docker/OCI registry client:
// bearer-token auth, mirror failover, manifest resolution, ranged blob
// reads and (best effort) blob upload for --push.
package registry

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoint is one registry API base address (a mirror or the origin).
type Endpoint struct {
	Name     string // human readable, used in logs
	Base     string // e.g. https://registry-1.docker.io
	Host     string // authority used for the Host header / auth service
	Insecure bool   // plain http
}

func (e Endpoint) String() string { return e.Name + " (" + e.Base + ")" }

// Credentials for a registry host.
type Credentials struct {
	Username string
	Password string
	Token    string // identity token / bearer token from docker login
}

// Error is a registry HTTP error with the parsed body attached.
type Error struct {
	StatusCode int
	Endpoint   string
	Op         string
	Body       string
	RetryAfter time.Duration
	Err        error // underlying cause, when there is one
}

// Unwrap keeps errors.Is/errors.As working through Error.
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s: HTTP %d", e.Op, e.Endpoint, e.StatusCode)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Retryable reports whether retrying the same request may help.
func (e *Error) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusTooEarly:
		return true
	}
	return e.StatusCode >= 500
}

// Client talks to a list of endpoints, in priority order.
type Client struct {
	HTTP      *http.Client
	Endpoints []Endpoint
	// CredentialsFor returns credentials for a host (may be zero).
	CredentialsFor func(host string) (Credentials, bool)
	// Logf receives diagnostics such as mirror failover decisions.
	Logf func(format string, args ...any)

	mu        sync.Mutex
	tokens    map[string]string     // base|repo|action -> token
	challenge map[string]*challenge // nil entry = endpoint needs no bearer token
	rejected  map[string]bool       // endpoints that failed hard for this session
}

// New builds a client for the given endpoints (tried in order).
func New(endpoints []Endpoint, cfg Config) *Client {
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 8
	}
	dialer := newIPPrefDial(cfg)
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          concurrency * 2,
		MaxIdleConnsPerHost:   concurrency * 2,
		MaxConnsPerHost:       concurrency * 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if cfg.SkipTLSVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		HTTP:      &http.Client{Transport: tr},
		Endpoints: endpoints,
		tokens:    map[string]string{},
		challenge: map[string]*challenge{},
		rejected:  map[string]bool{},
	}
}

// DisableEndpoint marks an endpoint as unusable for this run.
func (c *Client) DisableEndpoint(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejected[name] = true
}

func (c *Client) disabled(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejected[name]
}

type challenge struct {
	realm   string
	service string
	scope   string
	basic   bool // registry asked for Basic auth directly
}

// parseChallenge parses `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(h string) *challenge {
	h = strings.TrimSpace(h)
	i := strings.IndexAny(h, " \t")
	if i < 0 || !strings.EqualFold(h[:i], "Bearer") {
		return &challenge{basic: true}
	}
	ch := &challenge{}
	for _, f := range splitAuthHeader(strings.TrimSpace(h[i+1:])) {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.TrimSpace(k) {
		case "realm":
			ch.realm = v
		case "service":
			ch.service = v
		case "scope":
			ch.scope = v
		}
	}
	if ch.realm == "" {
		return &challenge{basic: true}
	}
	return ch
}

// splitAuthHeader splits on commas that are outside quotes.
func splitAuthHeader(h string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func escapePath(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func credsHeader(c Credentials) string {
	switch {
	case c.Token != "" && c.Username == "":
		return "Bearer " + c.Token
	case c.Username != "":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password))
	}
	return ""
}

// tokenFor fetches (or reuses) a token for the given endpoint/scope.
func (c *Client) tokenFor(ctx context.Context, ep Endpoint, repo, action string) (string, error) {
	key := ep.Base + "|" + repo + "|" + action
	c.mu.Lock()
	if t, ok := c.tokens[key]; ok {
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()

	ch, err := c.challengeFor(ctx, ep)
	if err != nil {
		return "", err
	}
	if ch == nil || ch.realm == "" || ch.basic {
		// no bearer token available: fall back to Basic/identity credentials
		return "", nil
	}
	debug("challenge %s realm=%q service=%q", ep.Name, ch.realm, ch.service)
	q := url.Values{}
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	scope := ch.scope
	if repo != "" && action != "" {
		scope = fmt.Sprintf("repository:%s:%s", repo, action)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u := ch.realm
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if c.CredentialsFor != nil {
		if cr, ok := c.CredentialsFor(ep.Host); ok {
			if h := credsHeader(cr); h != "" {
				req.Header.Set("Authorization", h)
			}
		}
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return "", &Error{StatusCode: res.StatusCode, Endpoint: ep.Name, Op: "token", Body: snippet(body)}
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("token %s: bad json: %w", ep.Name, err)
	}
	t := tok.Token
	if t == "" {
		t = tok.AccessToken
	}
	debug("token %s scope=%q len=%d", ep.Name, scope, len(t))
	c.mu.Lock()
	c.tokens[key] = t
	c.mu.Unlock()
	return t, nil
}

// challengeFor discovers the auth challenge of an endpoint. A nil result means
// the endpoint answered /v2/ without asking for authentication.
func (c *Client) challengeFor(ctx context.Context, ep Endpoint) (*challenge, error) {
	c.mu.Lock()
	if ch, ok := c.challenge[ep.Base]; ok {
		c.mu.Unlock()
		return ch, nil
	}
	c.mu.Unlock()

	var ch *challenge
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.Base+"/v2/", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		debug("ping %s 失败: %v", ep.Name, err)
		return nil, nil // unknown: proceed unauthenticated, the 401 path will fix it
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	debug("ping %s -> %d WWW-Authenticate=%q", ep.Name, res.StatusCode, res.Header.Get("WWW-Authenticate"))
	if a := res.Header.Get("WWW-Authenticate"); a != "" {
		ch = parseChallenge(a)
	}
	c.mu.Lock()
	c.challenge[ep.Base] = ch
	c.mu.Unlock()
	return ch, nil
}

// learnChallenge records the challenge advertised by a live 401 answer, which
// is more trustworthy than the /v2/ probe (proxies sometimes drop it).
func (c *Client) learnChallenge(ep Endpoint, header string) {
	if header == "" {
		return
	}
	ch := parseChallenge(header)
	if ch.realm == "" {
		return
	}
	c.mu.Lock()
	if old, ok := c.challenge[ep.Base]; !ok || old == nil || old.realm == "" {
		c.challenge[ep.Base] = ch
	}
	c.mu.Unlock()
}

// Request describes one registry API call.
type Request struct {
	Method   string
	Repo     string
	Action   string // pull | push
	Path     string // e.g. /v2/library/nginx/manifests/latest
	Headers  http.Header
	Failover bool // try the next endpoint on failure
	// Ok, when set, decides which status codes count as success.
	Ok func(code int) bool
	// Only pins the request to one endpoint instead of walking the list.
	Only *Endpoint
}

// Response couples a response with the endpoint that served it.
type Response struct {
	Endpoint Endpoint
	Resp     *http.Response
}

// Body closes over the response body helpers.
func (r *Response) Close() error { return r.Resp.Body.Close() }

func defaultOk(code int) bool { return code/100 == 2 }

// Do executes a request, walking the endpoint list when Failover is set.
func (c *Client) Do(ctx context.Context, rq Request) (*Response, error) {
	ok := rq.Ok
	if ok == nil {
		ok = defaultOk
	}
	var lastErr error
	for _, ep := range c.candidates(rq) {
		if rq.Failover && c.disabled(ep.Name) {
			continue
		}
		res, err := c.doOne(ctx, ep, rq, ok)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if rq.Failover && c.Logf != nil {
			c.Logf("%s 失败，切换下一个端点: %s", ep.Name, trimErr(err))
		}
		var re *Error
		if errors.As(err, &re) && !rq.Failover {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable registry endpoint for %s", rq.Path)
	}
	return nil, lastErr
}

// candidates returns the endpoints a request may use, in priority order.
func (c *Client) candidates(rq Request) []Endpoint {
	if rq.Only != nil {
		return []Endpoint{*rq.Only}
	}
	return c.Endpoints
}

func (c *Client) doOne(ctx context.Context, ep Endpoint, rq Request, ok func(int) bool) (*Response, error) {
	u := ep.Base + rq.Path
	var tok string
	if repo := rq.Repo; repo != "" {
		t, err := c.tokenFor(ctx, ep, repo, rq.Action)
		if err != nil {
			return nil, err
		}
		tok = t
	}
	resp, err := c.send(ctx, ep, rq, u, tok)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		c.learnChallenge(ep, wwwAuth)
		c.mu.Lock()
		delete(c.tokens, ep.Base+"|"+rq.Repo+"|"+rq.Action)
		c.mu.Unlock()
		if tok != "" {
			// the token we had was refused: do not loop, report it clearly
			return nil, &Error{StatusCode: resp.StatusCode, Endpoint: ep.Name,
				Op: rq.Method + " " + rq.Path,
				Body: "token rejected" + func() string {
					if wwwAuth != "" {
						return " (scope=" + trimErr(fmt.Errorf("%s", wwwAuth)) + ")"
					}
					return ""
				}()}
		}
		t, terr := c.tokenFor(ctx, ep, rq.Repo, rq.Action)
		if terr != nil {
			return nil, terr
		}
		if resp, err = c.send(ctx, ep, rq, u, t); err != nil {
			return nil, err
		}
	}
	if ok(resp.StatusCode) {
		return &Response{Endpoint: ep, Resp: resp}, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	ra := parseRetryAfter(resp.Header.Get("Retry-After"))
	resp.Body.Close()
	return nil, &Error{
		StatusCode: resp.StatusCode,
		Endpoint:   ep.Name,
		Op:         rq.Method + " " + rq.Path,
		Body:       snippet(body),
		RetryAfter: ra,
	}
}

func (c *Client) send(ctx context.Context, ep Endpoint, rq Request, u, tok string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, rq.Method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if rq.Method == http.MethodGet {
		req.Header.Set("Accept-Encoding", "identity")
	}
	for k, vs := range rq.Headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if c.CredentialsFor != nil {
		if cr, have := c.CredentialsFor(ep.Host); have {
			if h := credsHeader(cr); h != "" {
				req.Header.Set("Authorization", h)
			}
		}
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", rq.Method+" "+rq.Path, ep.Name, err)
	}
	return res, nil
}

// DoWithBody behaves like Do but attaches a streaming body.
func (c *Client) DoWithBody(ctx context.Context, rq Request, body io.Reader, contentLength int64) (*Response, error) {
	ok := rq.Ok
	if ok == nil {
		ok = defaultOk
	}
	var lastErr error
	for _, ep := range c.Endpoints {
		u := ep.Base + rq.Path
		tok := ""
		if rq.Repo != "" {
			t, err := c.tokenFor(ctx, ep, rq.Repo, rq.Action)
			if err != nil {
				lastErr = err
				continue
			}
			tok = t
		}
		req, err := http.NewRequestWithContext(ctx, rq.Method, u, body)
		if err != nil {
			return nil, err
		}
		if contentLength > 0 {
			req.ContentLength = contentLength
		}
		req.Header.Set("User-Agent", userAgent)
		for k, vs := range rq.Headers {
			req.Header.Del(k)
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		} else if c.CredentialsFor != nil {
			if cr, have := c.CredentialsFor(ep.Host); have {
				if h := credsHeader(cr); h != "" {
					req.Header.Set("Authorization", h)
				}
			}
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %s: %w", rq.Method, rq.Path, ep.Name, err)
			continue
		}
		if ok(res.StatusCode) {
			return &Response{Endpoint: ep, Resp: res}, nil
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		res.Body.Close()
		lastErr = &Error{StatusCode: res.StatusCode, Endpoint: ep.Name,
			Op: rq.Method + " " + rq.Path, Body: snippet(b)}
		if rq.Failover {
			continue
		}
		return nil, lastErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoint available for %s", rq.Path)
	}
	return nil, lastErr
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// debug prints protocol traces when DPULL_DEBUG=1.
func debug(format string, args ...any) {
	if os.Getenv("DPULL_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[registry] "+format+"\n", args...)
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return strings.ReplaceAll(s, "\n", " ")
}
