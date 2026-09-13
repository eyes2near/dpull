package registry

import (
	"fmt"
	"testing"
)

func TestParseChallenge(t *testing.T) {
	cases := []struct {
		in                    string
		realm, service, scope string
		basic                 bool
	}{
		{
			in:      `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`,
			realm:   "https://auth.docker.io/token",
			service: "registry.docker.io",
		},
		{
			in:      `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:user/image:pull"`,
			realm:   "https://ghcr.io/token",
			service: "ghcr.io",
			scope:   "repository:user/image:pull",
		},
		{
			in:    `Basic realm="Registry Realm"`,
			realm: "",
			basic: true,
		},
		{
			in:    `Bearer realm="https://r.example.com/token",service="r",error="denied",scope="a,b"`,
			realm: "https://r.example.com/token", service: "r", scope: "a,b",
		},
		{in: ``, basic: true},
	}
	for _, c := range cases {
		ch := parseChallenge(c.in)
		if ch.realm != c.realm || ch.service != c.service || ch.scope != c.scope || ch.basic != c.basic {
			t.Errorf("parseChallenge(%q) = %+v, want realm=%q service=%q scope=%q basic=%v",
				c.in, *ch, c.realm, c.service, c.scope, c.basic)
		}
	}
}

func TestParseContentRange(t *testing.T) {
	good := map[string][3]int64{
		"bytes 0-0/12345":       {0, 0, 12345},
		"bytes 1024-2047/99999": {1024, 2047, 99999},
		"bytes 0-9/*":           {0, 9, 0},
	}
	for in, want := range good {
		s, e, tot, _ := parseContentRange(in)
		if s != want[0] || e != want[1] || tot != want[2] {
			t.Errorf("parseContentRange(%q) = %d,%d,%d", in, s, e, tot)
		}
	}
	if _, _, _, ok := parseContentRange(""); ok {
		t.Error("empty Content-Range must not parse")
	}
}

func TestErrorRetryable(t *testing.T) {
	for _, code := range []int{429, 408, 500, 502, 503, 504} {
		if !(&Error{StatusCode: code}).Retryable() {
			t.Errorf("HTTP %d should be retryable", code)
		}
	}
	for _, code := range []int{401, 403, 404, 400} {
		if (&Error{StatusCode: code}).Retryable() {
			t.Errorf("HTTP %d should not be retryable", code)
		}
	}
}

func TestEscapePathAndBlobPath(t *testing.T) {
	if got := BlobPath("library/nginx", "sha256:aa"); got != "/v2/library/nginx/blobs/sha256:aa" {
		t.Errorf("BlobPath = %q", got)
	}
	if got := urlEscapeRef("sha256:abcdef12"); got != "sha256:abcdef12" {
		t.Errorf("urlEscapeRef = %q", got)
	}
}

func TestNeedsChunkedUpload(t *testing.T) {
	yes := []int{400, 411, 413, 415, 405}
	for _, code := range yes {
		err := fmt.Errorf("wrapped: %w", &Error{StatusCode: code})
		if !needsChunkedUpload(err) {
			t.Errorf("HTTP %d should fall back to a chunked upload", code)
		}
	}
	if needsChunkedUpload(&Error{StatusCode: 401}) {
		t.Error("an auth failure must not be retried as a chunked upload")
	}
}
