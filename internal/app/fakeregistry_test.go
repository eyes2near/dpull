package app_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// blobReq records one ranged blob read so tests can assert on resume behaviour.
type blobReq struct {
	digest  string
	start   int64
	end     int64
	aborted bool
	bytes   int64
}

type manifestRec struct {
	body      []byte
	digest    string
	mediaType string
}

// fakeRegistry is a minimal, misbehaviour-tolerant OCI registry used by tests.
type fakeRegistry struct {
	srv *httptest.Server

	blobs     map[string][]byte
	manifests map[string]manifestRec

	mu sync.Mutex
	// per (digest,start) attempt counters and failure switches
	abortFirst map[string]int
	abortAll   map[string]bool
	badData    map[string][]byte

	requests     []blobReq
	manifestGets []string
	closed       bool

	acceptRanges   bool
	advertiseRange bool // include Accept-Ranges on HEAD (default true)
	lieAboutRanges bool // advertises Accept-Ranges but ignores Range requests
	requireAuth    bool

	// push sink
	pushBlobs     map[string][]byte
	pushManifests map[string][]byte
	uploads       map[string]*bytes.Buffer
	nextUpload    int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{
		blobs:          map[string][]byte{},
		manifests:      map[string]manifestRec{},
		abortFirst:     map[string]int{},
		abortAll:       map[string]bool{},
		badData:        map[string][]byte{},
		pushBlobs:      map[string][]byte{},
		pushManifests:  map[string][]byte{},
		uploads:        map[string]*bytes.Buffer{},
		acceptRanges:   true,
		advertiseRange: true,
		requireAuth:    true,
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(func() {
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
		f.srv.Close()
	})
	return f
}

func (f *fakeRegistry) Host() string { return strings.TrimPrefix(f.srv.URL, "http://") }
func (f *fakeRegistry) URL() string  { return f.srv.URL }

func (f *fakeRegistry) lies() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lieAboutRanges
}

func (f *fakeRegistry) key(digest string, start int64) string {
	return fmt.Sprintf("%s@%d", digest, start)
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/v2/":
		if f.requireAuth && !f.authed(r) {
			f.challenge(w)
			return
		}
		w.WriteHeader(http.StatusOK)
	case p == "/token":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"token":"test-token"}`)
	case strings.HasPrefix(p, "/v2/test/img/manifests/"):
		f.serveManifest(w, r, strings.TrimPrefix(p, "/v2/test/img/manifests/"))
	case strings.HasPrefix(p, "/v2/test/img/blobs/"):
		f.serveBlob(w, r, strings.TrimPrefix(p, "/v2/test/img/blobs/"))
	case strings.HasPrefix(p, "/v2/ns/dst/blobs/uploads/"):
		f.startUpload(w, r)
	case strings.HasPrefix(p, "/upload/"):
		f.upload(w, r)
	case strings.HasPrefix(p, "/v2/ns/dst/blobs/"):
		f.headPushedBlob(w, strings.TrimPrefix(p, "/v2/ns/dst/blobs/"))
	case strings.HasPrefix(p, "/v2/ns/dst/manifests/"):
		f.putManifest(w, r, strings.TrimPrefix(p, "/v2/ns/dst/manifests/"))
	default:
		http.Error(w, `{"errors":[{"code":"NOT_FOUND","message":"no route"}]}`, http.StatusNotFound)
	}
}

func (f *fakeRegistry) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Bearer realm="http://%s/token",service="fake-registry"`, f.Host()))
	http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`,
		http.StatusUnauthorized)
}

func (f *fakeRegistry) authed(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer test-token"
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request, ref string) {
	if f.requireAuth && !f.authed(r) {
		f.challenge(w)
		return
	}
	f.mu.Lock()
	f.manifestGets = append(f.manifestGets, ref)
	m, ok := f.manifests[ref]
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Docker-Content-Digest", m.digest)
	w.Write(m.body)
}

func (f *fakeRegistry) serveBlob(w http.ResponseWriter, r *http.Request, digest string) {
	if f.requireAuth && !f.authed(r) {
		f.challenge(w)
		return
	}
	f.mu.Lock()
	data, ok := f.blobs[digest]
	if bad, badOK := f.badData[digest]; badOK {
		data = bad
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`, http.StatusNotFound)
		return
	}

	if r.Method == http.MethodHead {
		f.mu.Lock()
		advertise := f.advertiseRange
		f.mu.Unlock()
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if f.acceptRanges && advertise {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
		return
	}

	f.mu.Lock()
	acceptRanges := f.acceptRanges
	f.mu.Unlock()

	rng := r.Header.Get("Range")
	if f.lies() || !acceptRanges || rng == "" {
		// a non-ranged server still answers the probe with the full body
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		f.mu.Lock()
		f.requests = append(f.requests, blobReq{digest: digest, start: 0, end: int64(len(data)) - 1, bytes: int64(len(data))})
		f.mu.Unlock()
		return
	}

	var start, end int64
	if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
		http.Error(w, "bad range", http.StatusBadRequest)
		return
	}
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	// dpull probes range support with "bytes=0-0"; those reads are not part of
	// the transfer, so they are neither aborted nor recorded.
	if start == 0 && end == 0 && len(data) > 1 {
		body := data[0:1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(data)))
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
		return
	}

	key := f.key(digest, start)
	f.mu.Lock()
	n := f.abortFirst[key]
	if f.abortAll[digest] || f.abortAll[key] {
		n = 1 << 30
	} else if n > 0 {
		f.abortFirst[key] = n - 1
	}
	f.mu.Unlock()

	body := data[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	if n > 0 {
		// deliver a prefix, then drop the connection like a flaky mirror does
		half := len(body) / 2
		if half > 0 {
			_, _ = w.Write(body[:half])
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		f.mu.Lock()
		f.requests = append(f.requests, blobReq{digest: digest, start: start, end: end, aborted: true, bytes: int64(half)})
		f.mu.Unlock()
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(body)
	f.mu.Lock()
	f.requests = append(f.requests, blobReq{digest: digest, start: start, end: end, bytes: int64(len(body))})
	f.mu.Unlock()
}

func (f *fakeRegistry) startUpload(w http.ResponseWriter, r *http.Request) {
	if !f.authed(r) {
		f.challenge(w)
		return
	}
	f.mu.Lock()
	f.nextUpload++
	id := fmt.Sprintf("u%d", f.nextUpload)
	f.uploads[id] = &bytes.Buffer{}
	f.mu.Unlock()
	w.Header().Set("Location", "/upload/"+id)
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeRegistry) upload(w http.ResponseWriter, r *http.Request) {
	if !f.authed(r) {
		f.challenge(w)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/upload/")
	f.mu.Lock()
	buf, ok := f.uploads[id]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	if _, err := io.Copy(buf, r.Body); err != nil {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Location", "/upload/"+id)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	want := r.URL.Query().Get("digest")
	sum := sha256.Sum256(buf.Bytes())
	got := "sha256:" + hex.EncodeToString(sum[:])
	if want != "" && got != want {
		http.Error(w, fmt.Sprintf(`{"errors":[{"code":"DIGEST_INVALID","got":"%s","want":"%s"}]}`, got, want),
			http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.pushBlobs[want] = append([]byte(nil), buf.Bytes()...)
	f.mu.Unlock()
	w.Header().Set("Docker-Content-Digest", got)
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeRegistry) headPushedBlob(w http.ResponseWriter, digest string) {
	f.mu.Lock()
	_, ok := f.pushBlobs[digest]
	f.mu.Unlock()
	if ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (f *fakeRegistry) putManifest(w http.ResponseWriter, r *http.Request, tag string) {
	if !f.authed(r) {
		f.challenge(w)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.pushManifests[tag] = body
	f.mu.Unlock()
	sum := sha256.Sum256(body)
	w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
	w.WriteHeader(http.StatusCreated)
}

// snapshot / reset helpers

func (f *fakeRegistry) snapshotRequests() []blobReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]blobReq(nil), f.requests...)
}

func (f *fakeRegistry) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
	f.manifestGets = nil
}

func (f *fakeRegistry) resetCounters() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
	f.manifestGets = nil
	f.abortFirst = map[string]int{}
	f.abortAll = map[string]bool{}
}

func (f *fakeRegistry) failFirstAttemptPerChunk(times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for digest, data := range f.blobs {
		for s := int64(0); s < int64(len(data)); s += 1 << 15 {
			f.abortFirst[f.key(digest, s)] = times
		}
	}
}

// alwaysFail makes every read of a blob drop the connection, so that blob can
// never finish while the rest of the image still transfers fine.
func (f *fakeRegistry) alwaysFail(digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortAll[digest] = true
}

// ---- image fixtures ----

func makeLayer(t *testing.T, seed int64, size int) (gz []byte, diffID string) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	files := map[string][]byte{}
	steps := []string{"bin", "etc", "lib", "usr", "var", "opt"}
	for total := 0; total < size; {
		name := fmt.Sprintf("%s/file-%d", steps[len(files)%len(steps)], len(files))
		n := 4096 + rng.Intn(24576)
		if total+n > size {
			n = size - total
		}
		buf := make([]byte, n)
		rng.Read(buf)
		files[name] = buf
		total += n
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw.Bytes())
	diffID = "sha256:" + hex.EncodeToString(sum[:])
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), diffID
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

type testImage struct {
	manifest     []byte
	manifestMT   string
	manifestDig  string
	config       []byte
	configDig    string
	layers       [][]byte
	layerDigests []string
	diffIDs      []string
}

func makeImage(t *testing.T, layerSizes []int, arch string) *testImage {
	t.Helper()
	img := &testImage{manifestMT: "application/vnd.docker.distribution.manifest.v2+json"}
	for i, sz := range layerSizes {
		gz, diffID := makeLayer(t, int64(1000+i), sz)
		img.layers = append(img.layers, gz)
		img.layerDigests = append(img.layerDigests, digestOf(gz))
		img.diffIDs = append(img.diffIDs, diffID)
	}
	cfg := map[string]any{
		"architecture": arch,
		"os":           "linux",
		"config":       map[string]any{"Cmd": []string{"/bin/sh"}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": img.diffIDs},
		"history":      []any{},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	img.config = b
	img.configDig = digestOf(b)

	layers := make([]map[string]any, 0, len(img.layers))
	for i, gz := range img.layers {
		layers = append(layers, map[string]any{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"size":      len(gz),
			"digest":    img.layerDigests[i],
		})
	}
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     img.manifestMT,
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size":      len(img.config),
			"digest":    img.configDig,
		},
		"layers": layers,
	}
	img.manifest, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	img.manifestDig = digestOf(img.manifest)
	return img
}

func (f *fakeRegistry) addImage(t *testing.T, ref string, img *testImage) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[img.configDig] = img.config
	for i, gz := range img.layers {
		f.blobs[img.layerDigests[i]] = gz
	}
	f.manifests[ref] = manifestRec{body: img.manifest, digest: img.manifestDig, mediaType: img.manifestMT}
}
