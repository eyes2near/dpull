package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DefaultPushChunk is the PATCH chunk size used for chunked uploads.
const DefaultPushChunk = 16 << 20

// userAgent is sent with every request.
const userAgent = "dpull/1.0"

// BlobExists reports whether the target registry already has this blob.
func (c *Client) BlobExists(ctx context.Context, repo, digest string) (bool, error) {
	res, err := c.Do(ctx, Request{
		Method:   http.MethodHead,
		Repo:     repo,
		Action:   "pull",
		Path:     BlobPath(repo, digest),
		Failover: false,
		Ok:       func(code int) bool { return code == http.StatusOK || code == http.StatusNotFound },
	})
	if err != nil {
		return false, err
	}
	res.Close()
	return res.Resp.StatusCode == http.StatusOK, nil
}

// uploadTarget is an upload session location.
type uploadTarget struct {
	URL string
}

func (c *Client) ep0() Endpoint { return c.Endpoints[0] }

// resolveURL turns a (possibly relative) Location header into an absolute URL.
func resolveURL(ep Endpoint, location string) (string, error) {
	if location == "" {
		return "", fmt.Errorf("registry returned an empty upload Location")
	}
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return location, nil
	}
	base, err := url.Parse(ep.Base)
	if err != nil {
		return "", err
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(loc).String(), nil
}

// sameHost reports whether u points back at the registry API itself.
func sameHost(ep Endpoint, u string) bool {
	a, err1 := url.Parse(ep.Base)
	b, err2 := url.Parse(u)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(a.Host, b.Host)
}

func (c *Client) authHeaderFor(ctx context.Context, ep Endpoint, repo, action, u string) (string, error) {
	if !sameHost(ep, u) {
		return "", nil // storage backends (S3/OSS) must not receive our bearer token
	}
	tok, err := c.tokenFor(ctx, ep, repo, action)
	if err != nil {
		return "", err
	}
	if tok != "" {
		return "Bearer " + tok, nil
	}
	if c.CredentialsFor != nil {
		if cr, ok := c.CredentialsFor(ep.Host); ok {
			return credsHeader(cr), nil
		}
	}
	return "", nil
}

func (c *Client) raw(ctx context.Context, ep Endpoint, method, u string, hdr http.Header, body io.Reader, length int64, repo, action string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dpull/1.0")
	if length > 0 {
		req.ContentLength = length
	}
	for k, vs := range hdr {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Authorization") == "" {
		if a, err := c.authHeaderFor(ctx, ep, repo, action, u); err == nil && a != "" {
			req.Header.Set("Authorization", a)
		}
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, redactURL(u), err)
	}
	return res, nil
}

func redactURL(u string) string {
	if i := strings.Index(u, "?"); i > 0 {
		return u[:i] + "?<…>"
	}
	return u
}

// startUpload opens a blob upload session, optionally trying a cross-repository
// mount from fromRepo (same registry only).
func (c *Client) startUpload(ctx context.Context, ep Endpoint, repo, digest, fromRepo string) (uploadTarget, bool, error) {
	u := ep.Base + "/v2/" + escapePath(repo) + "/blobs/uploads/"
	if fromRepo != "" && fromRepo != repo {
		q := url.Values{}
		q.Set("mount", digest)
		q.Set("from", fromRepo)
		u += "?" + q.Encode()
	}
	res, err := c.raw(ctx, ep, http.MethodPost, u, nil, nil, 0, repo, "push,pull")
	if err != nil {
		return uploadTarget{}, false, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	if res.StatusCode == http.StatusCreated {
		// mount succeeded: blob is already there
		return uploadTarget{}, true, nil
	}
	if res.StatusCode != http.StatusAccepted {
		return uploadTarget{}, false, &Error{StatusCode: res.StatusCode, Endpoint: ep.Name,
			Op: "start upload", Body: snippet(body)}
	}
	loc, err := resolveURL(ep, res.Header.Get("Location"))
	if err != nil {
		return uploadTarget{}, false, err
	}
	return uploadTarget{URL: loc}, false, nil
}

func withParam(u, k, v string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + k + "=" + url.QueryEscape(v)
}

// PutBlobOptions controls one blob upload.
type PutBlobOptions struct {
	Repo     string
	Digest   string
	Size     int64
	FromRepo string // enable cross-repo mount from this repository
	Chunk    int64  // PATCH chunk size; 0 -> DefaultPushChunk
	// Open returns a fresh reader over the blob content.
	Open func() (io.ReadCloser, error)
	// Progress is fed with bytes accepted by the registry.
	Progress func(n int64)
}

// PushBlob uploads one blob, preferring a monolithic PUT and falling back to
// a chunked PATCH/PUT session when the registry demands it.
func (c *Client) PushBlob(ctx context.Context, o PutBlobOptions) error {
	ep := c.ep0()
	if o.Open == nil {
		return fmt.Errorf("PushBlob: Open is required")
	}
	if exists, err := c.BlobExists(ctx, o.Repo, o.Digest); err == nil && exists {
		if o.Progress != nil && o.Size > 0 {
			o.Progress(o.Size)
		}
		return nil
	}
	target, mounted, err := c.startUpload(ctx, ep, o.Repo, o.Digest, o.FromRepo)
	if err != nil {
		return err
	}
	if mounted {
		if o.Progress != nil && o.Size > 0 {
			o.Progress(o.Size)
		}
		return nil
	}
	err = c.putMonolithic(ctx, ep, target, o)
	if err == nil {
		return nil
	}
	if !needsChunkedUpload(err) {
		return err
	}
	return c.putChunked(ctx, ep, o)
}

// needsChunkedUpload decides whether a failed monolithic PUT should be
// retried as a PATCH session (registries that demand chunked uploads answer
// 400/411/413/415, and transport failures benefit from smaller pieces too).
func needsChunkedUpload(err error) bool {
	var re *Error
	if !errors.As(err, &re) {
		return true
	}
	switch re.StatusCode {
	case http.StatusBadRequest, http.StatusLengthRequired, http.StatusNotImplemented,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusMethodNotAllowed, http.StatusExpectationFailed:
		return true
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		return false
	}
	return re.StatusCode >= 500
}

func (c *Client) putMonolithic(ctx context.Context, ep Endpoint, target uploadTarget, o PutBlobOptions) error {
	rc, err := o.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	u := withParam(target.URL, "digest", o.Digest)
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/octet-stream")
	res, err := c.raw(ctx, ep, http.MethodPut, u, hdr, &counter{r: rc, fn: o.Progress}, o.Size, o.Repo, "push,pull")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	if res.StatusCode/100 == 2 {
		return nil
	}
	return &Error{StatusCode: res.StatusCode, Endpoint: ep.Name,
		Op: "put blob " + shortDigest(o.Digest), Body: snippet(body)}
}

// putChunked streams the blob with PATCH requests and closes the session with
// a final PUT. Registry-side resume is honoured via the Range header.
func (c *Client) putChunked(ctx context.Context, ep Endpoint, o PutBlobOptions) error {
	chunk := o.Chunk
	if chunk <= 0 {
		chunk = DefaultPushChunk
	}
	target, _, err := c.startUpload(ctx, ep, o.Repo, o.Digest, "")
	if err != nil {
		return err
	}
	rc, err := o.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	var sent int64
	buf := make([]byte, chunk)
	for {
		n, rerr := io.ReadFull(rc, buf)
		if n > 0 {
			last := false
			if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
				last = true
			} else if rerr != nil {
				return rerr
			}
			start, end := sent, sent+int64(n)-1
			total := "*"
			if o.Size > 0 {
				total = strconv.FormatInt(o.Size, 10)
			}
			if last || (o.Size > 0 && sent+int64(n) >= o.Size) {
				if err := c.finishUpload(ctx, ep, target.URL, o, buf[:n], start, end, total); err != nil {
					return err
				}
				return nil
			}
			hdr := http.Header{}
			hdr.Set("Content-Type", "application/octet-stream")
			hdr.Set("Content-Range", fmt.Sprintf("%d-%d", start, end))
			res, err := c.raw(ctx, ep, http.MethodPatch, target.URL, hdr,
				&counter{r: newBytesReader(buf[:n]), fn: o.Progress}, int64(n), o.Repo, "push,pull")
			if err != nil {
				return err
			}
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
			code := res.StatusCode
			if loc := res.Header.Get("Location"); loc != "" {
				if abs, e := resolveURL(ep, loc); e == nil {
					target.URL = abs
				}
			}
			res.Body.Close()
			if code != http.StatusAccepted {
				return &Error{StatusCode: code, Endpoint: ep.Name, Op: "patch blob",
					Body: snippet(body)}
			}
			sent += int64(n)
			if rerr == io.EOF {
				break
			}
			continue
		}
		// nothing read: EOF, close the session
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return c.finishUpload(ctx, ep, target.URL, o, nil, sent, sent-1, func() string {
				if o.Size > 0 {
					return strconv.FormatInt(o.Size, 10)
				}
				return "*"
			}())
		}
		return rerr
	}
	return fmt.Errorf("chunked upload ended unexpectedly")
}

func (c *Client) finishUpload(ctx context.Context, ep Endpoint, u string, o PutBlobOptions, tail []byte, start, end int64, total string) error {
	var body io.Reader
	length := int64(0)
	if len(tail) > 0 {
		body = newBytesReader(tail)
		length = int64(len(tail))
	}
	hdr := http.Header{}
	if length > 0 {
		hdr.Set("Content-Range", fmt.Sprintf("%d-%d/%s", start, end, total))
	}
	hdr.Set("Content-Type", "application/octet-stream")
	u2 := withParam(u, "digest", o.Digest)
	res, err := c.raw(ctx, ep, http.MethodPut, u2, hdr, &counter{r: body, fn: o.Progress}, length, o.Repo, "push,pull")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	if res.StatusCode/100 == 2 {
		return nil
	}
	return &Error{StatusCode: res.StatusCode, Endpoint: ep.Name,
		Op: "finish upload " + shortDigest(o.Digest), Body: snippet(b)}
}

// PutManifest uploads an image manifest under tag (or digest reference).
func (c *Client) PutManifest(ctx context.Context, repo, ref string, body []byte, mediaType string) error {
	ep := c.ep0()
	u := ep.Base + "/v2/" + escapePath(repo) + "/manifests/" + urlEscapeRef(ref)
	hdr := http.Header{}
	hdr.Set("Content-Type", mediaType)
	res, err := c.raw(ctx, ep, http.MethodPut, u, hdr, newBytesReader(body), int64(len(body)), repo, "push,pull")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	if res.StatusCode/100 == 2 {
		return nil
	}
	return &Error{StatusCode: res.StatusCode, Endpoint: ep.Name,
		Op: "put manifest " + ref, Body: snippet(b)}
}

// newBytesReader returns a resumable reader over b.
func newBytesReader(b []byte) io.Reader {
	if len(b) == 0 {
		return nil
	}
	return bytes.NewReader(b)
}

type counter struct {
	r   io.Reader
	fn  func(int64)
	buf []byte
}

func (c *counter) Read(p []byte) (int, error) {
	if c.r == nil {
		return 0, io.EOF
	}
	n, err := c.r.Read(p)
	if n > 0 && c.fn != nil {
		c.fn(int64(n))
	}
	return n, err
}
