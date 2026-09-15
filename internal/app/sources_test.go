package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dpull/internal/reference"
	"dpull/internal/registry"
)

// stubSource is a minimal registry: no auth, one small layer, correct digests.
type stubSource struct {
	srv             *httptest.Server
	hits            int
	manifestUnknown bool
	body            []byte
	blobDigest      string
}

func newStubSource(t *testing.T, manifestUnknown bool) *stubSource {
	t.Helper()
	s := &stubSource{manifestUnknown: manifestUnknown}
	s.body = []byte(strings.Repeat("layer-bytes-", 200))
	sum := sha256.Sum256(s.body)
	s.blobDigest = "sha256:" + hex.EncodeToString(sum[:])
	cfg := []byte(`{"architecture":"arm64","os":"linux"}`)
	csum := sha256.Sum256(cfg)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits++
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/manifests/v1"):
			if s.manifestUnknown {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
				return
			}
			m := map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
				"config": map[string]any{
					"mediaType": "application/vnd.docker.container.image.v1+json",
					"digest":    "sha256:" + hex.EncodeToString(csum[:]), "size": len(cfg),
				},
				"layers": []any{map[string]any{
					"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
					"digest":    s.blobDigest, "size": len(s.body),
				}},
			}
			b, _ := json.Marshal(m)
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sha256sum(b))[:])
			w.WriteHeader(http.StatusOK)
			w.Write(b)
		case strings.Contains(r.URL.Path, "/blobs/sha256:"):
			if strings.Contains(r.URL.Path, s.blobDigest) {
				w.WriteHeader(http.StatusOK)
				w.Write(s.body)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(cfg)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func sha256sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// URL is the base address a fallback source would be configured with.
func (s *stubSource) URL() string { return s.srv.URL }

// Host is the host:port a reference would be written with.
func (s *stubSource) Host() string { return strings.TrimPrefix(s.srv.URL, "http://") }

// unreachableRef points at a port nothing listens on, which reproduces the
// failure mode that matters here: we never get an answer at all.
func unreachableRef(t *testing.T) reference.Ref {
	t.Helper()
	ref, err := reference.Parse("127.0.0.1:1/library/img:v1")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func anyRepo(r reference.Ref) (string, bool) { return r.Repository, true }

func TestFallbackSourceUsedWhenOriginUnreachable(t *testing.T) {
	src := newStubSource(t, false)
	restore := fallbackSources
	fallbackSources = []fallbackSource{{Name: "stub", Base: src.URL(), Repo: anyRepo}}
	defer func() { fallbackSources = restore }()

	dir := filepath.Join(t.TempDir(), "cache")
	o := &Options{CacheDir: dir, Concurrency: 4, ChunkSize: 1 << 20, Retries: 1, Stall: 2 * time.Second, Quiet: true}
	ref := unreachableRef(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, res, err := resolveWithSources(ctx, o, ref, "v1", "", false)
	if err != nil {
		t.Fatalf("fallback did not rescue the pull: %v", err)
	}
	if cli == nil || res == nil {
		t.Fatal("missing client/resolved")
	}
	if len(res.Layers) != 1 {
		t.Fatalf("layers = %d", len(res.Layers))
	}
	if src.hits == 0 {
		t.Error("the fallback source was never contacted")
	}
	// the winner must be remembered so the next run does not waste time on the
	// dead origin first
	b, err := os.ReadFile(sourceMemoryPath(dir))
	if err != nil {
		t.Fatalf("no source memory written: %v", err)
	}
	if !strings.Contains(string(b), `"stub"`) {
		t.Errorf("source memory = %s", b)
	}
}

func TestFallbackNotUsedForADeliberateAnswer(t *testing.T) {
	// The origin answers on the merits this time: "I don't have that tag".
	origin := newStubSource(t, true)
	cache := newStubSource(t, false)
	restore := fallbackSources
	fallbackSources = []fallbackSource{{Name: "stub", Base: cache.URL(), Repo: anyRepo}}
	defer func() { fallbackSources = restore }()

	ref, err := reference.Parse(origin.Host() + "/library/img:v1")
	if err != nil {
		t.Fatal(err)
	}
	// both stubs speak plain http on loopback, which --insecure permits
	o := &Options{CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2, ChunkSize: 1 << 20, Retries: 0, Stall: 2 * time.Second, Quiet: true, Insecure: true}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, _, err = resolveWithSources(ctx, o, ref, "v1", "", false)
	if err == nil {
		t.Fatal("expected the manifest-unknown answer to fail")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("the real answer must reach the user, got %v", err)
	}
	// Retrying an unknown tag against a cache could resolve a typo into some
	// other build, so the built-in source must not even be contacted.
	if cache.hits != 0 {
		t.Errorf("a cache must not be consulted for a real answer, got %d hits", cache.hits)
	}
}

func TestNoAutoSourceHonoured(t *testing.T) {
	src := newStubSource(t, false)
	restore := fallbackSources
	fallbackSources = []fallbackSource{{Name: "stub", Base: src.URL(), Repo: anyRepo}}
	defer func() { fallbackSources = restore }()

	o := &Options{CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2, ChunkSize: 1 << 20, Retries: 0, Stall: 2 * time.Second, Quiet: true, NoAutoSource: true}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, _, err := resolveWithSources(ctx, o, unreachableRef(t), "v1", "", false); err == nil {
		t.Fatal("expected failure with auto-source disabled")
	}
	if src.hits != 0 {
		t.Errorf("built-in sources must stay untouched with --no-auto-source, got %d hits", src.hits)
	}
}

func TestPreferBuiltinsTriesBuiltinsFirst(t *testing.T) {
	src := newStubSource(t, false)
	restore := fallbackSources
	fallbackSources = []fallbackSource{{Name: "stub", Base: src.URL(), Repo: anyRepo}}
	defer func() { fallbackSources = restore }()

	o := &Options{CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2, ChunkSize: 1 << 20, Retries: 0, Stall: 2 * time.Second, Quiet: true}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The reference is served by the stub only through the rewrite, so a success
	// proves the built-in was consulted before giving up on the origin.
	ref, err := reference.Parse("127.0.0.1:1/library/img:v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveWithSources(ctx, o, ref, "v1", "", true); err != nil {
		t.Fatalf("prefer-builtin path failed: %v", err)
	}
	if src.hits == 0 {
		t.Error("built-in source was not tried first")
	}
}

func TestSourceMemoryOrderingAndExpiry(t *testing.T) {
	dir := t.TempDir()
	saveSourceMemory(dir, "registry-1.docker.io", "xuanyuan")
	ordered := orderedFallbacks(dir, "registry-1.docker.io")
	if ordered[0].Name != "xuanyuan" {
		t.Fatalf("remembered winner not first: %+v", ordered[0])
	}
	if len(ordered) != len(fallbackSources) {
		t.Fatalf("ordering lost sources: %d vs %d", len(ordered), len(fallbackSources))
	}
	// an expired memory entry must not pin a stale choice
	b, _ := json.Marshal(map[string]sourceMemory{
		"registry-1.docker.io": {Origin: "registry-1.docker.io", Source: "xuanyuan", At: time.Now().Add(-2 * sourceMemoryTTL)},
	})
	if err := os.WriteFile(sourceMemoryPath(dir), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := orderedFallbacks(dir, "registry-1.docker.io"); got[0].Name != fallbackSources[0].Name {
		t.Errorf("stale memory still reordered sources: %+v", got[0])
	}
}

func TestRepoMappingRules(t *testing.T) {
	hub := reference.Ref{Registry: reference.DefaultRegistry, Repository: "library/nginx"}
	other := reference.Ref{Registry: "quay.io", Repository: "prometheus/prometheus"}
	gcr := reference.Ref{Registry: "gcr.io", Repository: "distroless/base"}
	if repo, ok := hubMirror(hub); !ok || repo != "library/nginx" {
		t.Errorf("hubMirror(hub) = %q %v", repo, ok)
	}
	if _, ok := hubMirror(other); ok {
		t.Error("hubMirror must not claim non-hub references")
	}
	if repo, ok := ecrAlias(hub); !ok || repo != "docker/library/nginx" {
		t.Errorf("ecrAlias(hub) = %q %v", repo, ok)
	}
	if _, ok := ecrAlias(other); ok {
		t.Error("ecrAlias must not claim non-hub references")
	}
	// A rewrite cache only serves its own upstream, and passes the path through.
	gcrOnly := hostMirror("gcr.io")
	if repo, ok := gcrOnly(gcr); !ok || repo != "distroless/base" {
		t.Errorf("hostMirror(gcr.io)(gcr) = %q %v", repo, ok)
	}
	for _, r := range []reference.Ref{hub, other} {
		if _, ok := gcrOnly(r); ok {
			t.Errorf("hostMirror(gcr.io) claimed %s", r.Host())
		}
	}
	// k8s.gcr.io is deliberately *not* an alias here: an alias we cannot verify
	// against the origin would turn the cache into the only authority on the tag.
	if _, ok := gcrOnly(reference.Ref{Registry: "k8s.gcr.io", Repository: "pause"}); ok {
		t.Error("unverified upstream aliases must not be mapped")
	}
}

// TestFallbackTableCoversUpstreamsWithoutCrossTalking is the regression test for
// "the built-in table only knew Docker Hub": every rewrite entry must serve the
// upstream it advertises, must refuse every other upstream in the table, and
// must be labelled well enough for a log line to say who served what.
func TestFallbackTableCoversUpstreamsWithoutCrossTalking(t *testing.T) {
	hosts := map[string]bool{}
	for _, s := range fallbackSources {
		if s.Upstream != "" {
			hosts[s.Upstream] = true
		}
	}
	if len(hosts) < 4 {
		t.Errorf("rewrite caches only cover %d upstreams; Docker Hub must not be the only covered registry", len(hosts))
	}
	for _, s := range fallbackSources {
		if s.Name == "" || s.Base == "" || s.Repo == nil || s.Note == "" {
			t.Fatalf("incomplete source entry: %+v", s)
		}
		if s.Provider == "" {
			t.Fatalf("source %q has no Provider, so log lines will be unhelpful", s.Name)
		}
		if s.Upstream == "" {
			continue // hub table: covered by hubMirror/ecrAlias above
		}
		if !strings.HasPrefix(s.Base, "https://") {
			t.Errorf("source %q is not https: %s", s.Name, s.Base)
		}
		label := s.Label()
		if !strings.Contains(label, s.Upstream) || !strings.Contains(label, s.Provider) {
			t.Errorf("Label(%q) = %q: must name both provider and upstream", s.Name, label)
		}
		for host := range hosts {
			r := reference.Ref{Registry: host, Repository: "someone/thing"}
			repo, ok := s.Repo(r)
			want := host == s.Upstream
			if ok != want {
				t.Errorf("source %q claims %s = %v, want %v", s.Name, host, ok, want)
			}
			if ok && repo != r.Repository {
				t.Errorf("source %q rewrote %s to %q: a rewrite cache must pass the path through", s.Name, r.Repository, repo)
			}
		}
		// The Docker Hub table must stay out of the way of rewrite caches, and
		// vice versa: a hub source answering for gcr.io would fetch a different
		// repository than the one the user asked for.
		if s.Upstream != "" {
			if _, ok := s.Repo(reference.Ref{Registry: reference.DefaultRegistry, Repository: "library/nginx"}); ok {
				t.Errorf("source %q claims Docker Hub", s.Name)
			}
		}
	}
}

// TestFallbackOnlySourcesThatCanServeTheUpstream proves the wiring that matters:
// when the origin is unreachable, a source that cannot serve that upstream must
// not even be contacted, and the one that can must be the one that rescues it.
// Everything stays on loopback so the test never needs a network.
func TestFallbackOnlySourcesThatCanServeTheUpstream(t *testing.T) {
	hubOnly := newStubSource(t, false)
	upstreamSrc := newStubSource(t, false)
	restore := fallbackSources
	fallbackSources = []fallbackSource{
		{Name: "hub", Base: hubOnly.URL(), Repo: hubMirror, Provider: "hubstub"},
		{Name: "k8s", Base: upstreamSrc.URL(), Repo: hostMirror("127.0.0.1:1"), Provider: "k8sstub", Upstream: "127.0.0.1:1"},
	}
	defer func() { fallbackSources = restore }()

	o := &Options{CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2, ChunkSize: 1 << 20, Retries: 0, Stall: 2 * time.Second, Quiet: true, Insecure: true}
	ref := unreachableRef(t) // 127.0.0.1:1 — nothing listens there
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, _, err := resolveWithSources(ctx, o, ref, "v1", "", false); err != nil {
		t.Fatalf("the source that serves this upstream should have rescued the pull: %v", err)
	}
	if hubOnly.hits != 0 {
		t.Errorf("a Docker-Hub-only cache was contacted for another upstream (%d hits)", hubOnly.hits)
	}
	if upstreamSrc.hits == 0 {
		t.Error("the source that serves this upstream was not contacted")
	}
	// The winner is remembered per upstream host, so the next run starts there.
	b, err := os.ReadFile(sourceMemoryPath(o.CacheDir))
	if err != nil {
		t.Fatalf("no source memory written: %v", err)
	}
	if !strings.Contains(string(b), `"k8s"`) || !strings.Contains(string(b), "127.0.0.1:1") {
		t.Errorf("source memory = %s", b)
	}
}

func TestSourceLabelNamesTheUpstream(t *testing.T) {
	restore := fallbackSources
	defer func() { fallbackSources = restore }()
	fallbackSources = []fallbackSource{upstreamSource("x-gcr", "https://x.invalid", "Acme", "gcr.io")}
	got := sourceLabel("内置:x-gcr")
	if !strings.Contains(got, "gcr.io") || !strings.Contains(got, "Acme") {
		t.Errorf("sourceLabel = %q", got)
	}
	if got := sourceLabel("内置:unknown-name"); !strings.Contains(got, "unknown-name") {
		t.Errorf("unknown sources must still be named: %q", got)
	}
}

func TestIsNetworkError(t *testing.T) {
	yes := []string{
		`Get "https://registry-1.docker.io/v2/": read tcp 10.0.0.2:52000->1.2.3.4:443: read: connection reset by peer`,
		`dial tcp 128.242.250.155:443: i/o timeout`,
		`dial tcp [2a03:2880::1]:443: connect: no route to host`,
		`net/http: TLS handshake timeout`,
		`Get "https://x/v2/": context deadline exceeded`,
	}
	for _, m := range yes {
		if !isNetworkError(fmt.Errorf("%s", m)) {
			t.Errorf("want network error: %s", m)
		}
	}
	no := []string{
		`manifest unknown`,
		`unauthorized: authentication required`,
		`HTTP 404: {"errors":[{"code":"MANIFEST_UNKNOWN"}]}`,
		`invalid reference format`,
	}
	for _, m := range no {
		if isNetworkError(fmt.Errorf("%s", m)) {
			t.Errorf("not a network error: %s", m)
		}
	}
}

func TestManifestDigestFallback(t *testing.T) {
	want := "sha256:" + hex.EncodeToString(sha256sum([]byte("manifest-body")))
	got := manifestDigest(&registry.Resolved{ManifestBytes: []byte("manifest-body")})
	if got != want {
		t.Errorf("manifestDigest = %q want %q", got, want)
	}
	if manifestDigest(&registry.Resolved{ManifestDigest: "sha256:aaa"}) != "sha256:aaa" {
		t.Error("a reported digest must win over hashing")
	}
	if manifestDigest(&registry.Resolved{IndexDigest: "sha256:bbb"}) != "sha256:bbb" {
		t.Error("the tag digest must be used when the platform digest is absent")
	}
	if manifestDigest(&registry.Resolved{}) != "" {
		t.Error("an empty resolution must yield no digest instead of a bogus one")
	}
}
