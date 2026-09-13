package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrIgnoredRange marks a server that advertises range support but returns the
// full body anyway.
var ErrIgnoredRange = errors.New("server ignored Range header")

func header(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}

// BlobPath builds the API path of a blob.
func BlobPath(repo, digest string) string {
	return "/v2/" + escapePath(repo) + "/blobs/" + digest
}

// BlobLocation is everything needed to download one blob repeatedly.
type BlobLocation struct {
	Endpoint Endpoint
	Repo     string
	Digest   string
	Size     int64 // -1 when unknown
	Ranged   bool  // server honours Range requests
	// ExternalURLs are extra download locations (OCI `urls`), tried first.
	ExternalURLs []string
}

func (b BlobLocation) String() string {
	return b.Endpoint.Name + b.Repo + "@" + shortDigest(b.Digest)
}

// shortDigest trims a digest for logs.
func shortDigest(d string) string {
	if i := strings.LastIndex(d, ":"); i >= 0 && len(d)-i > 16 {
		return d[:i+16]
	}
	return d
}

// ShortDigest is the exported form of shortDigest.
func ShortDigest(d string) string { return shortDigest(d) }

// LocateBlob discovers a blob's size and whether ranged reads really work.
//
// The Range capability is probed with an actual "bytes=0-0" GET rather than
// trusted from the HEAD answer: several large registries (Azure/MCR among them)
// serve correct 206 responses while omitting Accept-Ranges on HEAD, and trusting
// the header would silently degrade a whole image to one connection per layer.
func (c *Client) LocateBlob(ctx context.Context, repo, digest string, urls []string) (BlobLocation, error) {
	loc := BlobLocation{Repo: repo, Digest: digest, Size: -1}
	loc.ExternalURLs = append(loc.ExternalURLs, urls...)

	var hint bool
	res, err := c.Do(ctx, Request{
		Method:   http.MethodHead,
		Repo:     repo,
		Action:   "pull",
		Path:     BlobPath(repo, digest),
		Failover: true,
	})
	if err == nil {
		loc.Endpoint = res.Endpoint
		loc.Size = contentLength(res.Resp)
		hint = acceptsRanges(res.Resp)
		res.Close()
	} else if !isNotFound(err) {
		debug("HEAD %s 失败: %s", shortDigest(digest), trimErr(err))
	}

	cands := c.Endpoints
	if loc.Endpoint.Base != "" {
		cands = []Endpoint{loc.Endpoint}
	}
	var lastErr error
	for i := range cands {
		ep := cands[i]
		ranged, total, perr := c.probeRange(ctx, ep, repo, digest)
		if perr != nil {
			lastErr = perr
			continue
		}
		loc.Endpoint = ep
		loc.Ranged = ranged
		if total > 0 {
			loc.Size = total
		}
		debug("blob %s @%s size=%d ranged=%v (HEAD 声称 ranged=%v)", shortDigest(digest), ep.Name, loc.Size, ranged, hint)
		return loc, nil
	}
	if loc.Endpoint.Base != "" && loc.Size > 0 {
		// HEAD worked but the probe failed: streaming the whole blob is safe.
		loc.Ranged = false
		return loc, nil
	}
	if lastErr != nil {
		return BlobLocation{}, lastErr
	}
	return BlobLocation{}, fmt.Errorf("blob %s not found in %s", shortDigest(digest), repo)
}

// probeRange asks for a single byte and reports whether the server answered
// with 206, along with the total size when the server reveals it.
func (c *Client) probeRange(ctx context.Context, ep Endpoint, repo, digest string) (ranged bool, total int64, err error) {
	res, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		Repo:    repo,
		Action:  "pull",
		Path:    BlobPath(repo, digest),
		Headers: header("Range", "bytes=0-0"),
		Only:    &ep,
		Ok: func(code int) bool {
			return code == http.StatusOK || code == http.StatusPartialContent
		},
	})
	if err != nil {
		return false, 0, err
	}
	defer res.Close()
	if _, _, t, ok := parseContentRange(res.Resp.Header.Get("Content-Range")); ok && t > 0 {
		total = t
	}
	buf := make([]byte, 1)
	_, _ = res.Resp.Body.Read(buf)
	if res.Resp.StatusCode == http.StatusPartialContent {
		return true, total, nil
	}
	return false, contentLength(res.Resp), nil
}

func isNotFound(err error) bool {
	var re *Error
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound
}

func contentLength(r *http.Response) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	if v := r.Header.Get("Content-Length"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return -1
}

func acceptsRanges(r *http.Response) bool {
	if strings.Contains(strings.ToLower(r.Header.Get("Accept-Ranges")), "bytes") {
		return true
	}
	return r.StatusCode == http.StatusPartialContent ||
		r.Header.Get("Content-Range") != ""
}

// parseContentRange parses "bytes 123-456/789".
func parseContentRange(v string) (start, end, total int64, ok bool) {
	if v == "" {
		return 0, 0, 0, false
	}
	v = strings.TrimPrefix(v, "bytes ")
	if i := strings.Index(v, "/"); i >= 0 {
		tail := v[i+1:]
		v = v[:i]
		if tail != "*" {
			var t int64
			if _, err := fmt.Sscanf(tail, "%d", &t); err == nil {
				total = t
				ok = true
			}
		}
	}
	var s, e int64
	if _, err := fmt.Sscanf(v, "%d-%d", &s, &e); err != nil {
		return 0, 0, total, ok
	}
	return s, e, total, ok || total > 0
}

// OpenRange streams [start, end] of a blob. end < 0 means "to the end".
// The returned response body must be closed by the caller.
func (c *Client) OpenRange(ctx context.Context, loc BlobLocation, start, end int64) (*Response, error) {
	var h http.Header
	if start > 0 || end >= 0 {
		rng := fmt.Sprintf("bytes=%d-", start)
		if end >= 0 {
			rng = fmt.Sprintf("bytes=%d-%d", start, end)
		}
		h = header("Range", rng)
	}
	res, err := c.Do(ctx, Request{
		Method:   http.MethodGet,
		Repo:     loc.Repo,
		Action:   "pull",
		Path:     BlobPath(loc.Repo, loc.Digest),
		Headers:  h,
		Failover: true,
		Ok: func(code int) bool {
			return code == http.StatusOK || code == http.StatusPartialContent
		},
	})
	if err != nil {
		return nil, err
	}
	// A server that ignores our Range header cannot be used to fill a chunk
	// in the middle of a file: reject it so the caller can degrade to a
	// single sequential stream instead of writing garbage.
	if h != nil && res.Resp.StatusCode == http.StatusOK && start > 0 {
		res.Close()
		return nil, &Error{StatusCode: http.StatusOK, Endpoint: res.Endpoint.Name,
			Op: "range", Body: "server ignored Range header", Err: ErrIgnoredRange}
	}
	return res, nil
}

// Get is a small helper returning a whole (small) response body.
func (c *Client) Get(ctx context.Context, repo, action, path string, hdr http.Header) ([]byte, string, error) {
	res, err := c.Do(ctx, Request{Method: http.MethodGet, Repo: repo, Action: action,
		Path: path, Headers: hdr, Failover: true})
	if err != nil {
		return nil, "", err
	}
	defer res.Close()
	b, err := io.ReadAll(res.Resp.Body)
	return b, res.Resp.Header.Get("Content-Type"), err
}
