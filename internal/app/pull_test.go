package app_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dpull/internal/app"
)

func baseOptions(t *testing.T, f *fakeRegistry) *app.Options {
	t.Helper()
	return &app.Options{
		Images:      []string{f.Host() + "/test/img:v1"},
		CacheDir:    filepath.Join(t.TempDir(), "cache"),
		Format:      "docker-archive",
		Load:        false,
		DockerBin:   "definitely-not-docker",
		Concurrency: 8,
		ChunkSize:   32 << 10,
		Retries:     5,
		RetryWait:   20 * time.Millisecond,
		Stall:       4 * time.Second,
		Quiet:       true,
		Insecure:    true,
	}
}

func TestPullWritesValidDockerArchive(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{180000, 240000, 90000}, "arm64")
	f.addImage(t, "v1", img)

	o := baseOptions(t, f)
	out := filepath.Join(t.TempDir(), "nested", "img.tar")
	o.Output = out
	o.KeepArchive = true

	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got := readArchive(t, out)

	if len(got.layers) != len(img.layers) {
		t.Fatalf("layer count = %d, want %d", len(got.layers), len(img.layers))
	}
	if !bytes.Equal(got.config, img.config) {
		t.Fatal("config blob content differs from the registry copy")
	}
	for i, want := range img.layers {
		gz := got.layers[i]
		if !bytes.Equal(gz, want) {
			t.Fatalf("layer %d bytes differ (got %d, want %d)", i+1, len(gz), len(want))
		}
		// docker load decompresses each layer file and compares against
		// rootfs.diff_ids, so the archive is only valid if this matches.
		if got := gunzipDigest(t, gz); got != img.diffIDs[i] {
			t.Fatalf("layer %d diffID = %s, want %s", i+1, got, img.diffIDs[i])
		}
	}
	if got.manifestJSON[0].Config != hexName(img.configDig)+".json" {
		t.Errorf("manifest.json Config = %q", got.manifestJSON[0].Config)
	}
	wantLayers := []string{}
	for _, d := range img.layerDigests {
		wantLayers = append(wantLayers, hexName(d)+".tar")
	}
	if strings.Join(got.manifestJSON[0].Layers, ",") != strings.Join(wantLayers, ",") {
		t.Errorf("manifest.json Layers = %v, want %v", got.manifestJSON[0].Layers, wantLayers)
	}
	if !strings.HasSuffix(strings.Join(got.manifestJSON[0].RepoTags, ","), "/test/img:v1") {
		t.Errorf("RepoTags = %v", got.manifestJSON[0].RepoTags)
	}
	// the raw registry manifest must round-trip so --push can re-upload it
	if !bytes.Equal(got.rawManifest, img.manifest) {
		t.Error("dpull-manifest.json does not match the registry manifest")
	}

	// every byte downloaded is the exact size, no duplicated ranges
	var total int64
	for _, r := range f.snapshotRequests() {
		total += r.bytes
	}
	if total != img.totalSize() {
		t.Errorf("registry served %d bytes, image is %d", total, img.totalSize())
	}
}

func TestRetriesResumeInsideChunk(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{300000, 120000}, "amd64")
	f.addImage(t, "v1", img)
	f.failFirstAttemptPerChunk(1)

	o := baseOptions(t, f)
	out := filepath.Join(t.TempDir(), "img.tar")
	o.Output = out
	o.KeepArchive = true

	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("pull with flaky mirror: %v", err)
	}
	got := readArchive(t, out)
	for i, want := range img.layers {
		if !bytes.Equal(got.layers[i], want) {
			t.Fatalf("layer %d corrupted after retries", i+1)
		}
	}

	reqs := f.snapshotRequests()
	aborted := 0
	resumed := 0
	for _, r := range reqs {
		if r.aborted {
			aborted++
			continue
		}
		for _, a := range reqs {
			if a.aborted && a.digest == r.digest && r.start == a.start+a.bytes {
				resumed++
			}
		}
	}
	if aborted == 0 {
		t.Fatal("expected the mirror to drop some connections")
	}
	if resumed == 0 {
		t.Error("never resumed a partially received chunk; it restarted from scratch")
	}
	var total int64
	for _, r := range reqs {
		total += r.bytes
	}
	if total != img.totalSize() {
		t.Errorf("transfer size = %d, want %d (bytes received twice or lost)", total, img.totalSize())
	}
}

func TestResumeAcrossRunsKeepsProgress(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{200000, 200000}, "amd64")
	f.addImage(t, "v1", img)

	o := baseOptions(t, f)
	out := filepath.Join(t.TempDir(), "img.tar")
	o.Output = out
	o.KeepArchive = true

	// first run: the second layer never completes -> the pull must fail
	f.alwaysFail(img.layerDigests[1])
	if _, err := app.Pull(context.Background(), o); err == nil {
		t.Fatal("expected the interrupted pull to fail")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("archive was produced by a failed pull")
	}
	firstRun := f.snapshotRequests()
	if len(firstRun) == 0 {
		t.Fatal("no blob traffic recorded")
	}

	// second run: mirror is healthy again, progress must be reused
	f.resetCounters()
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("resumed pull: %v", err)
	}
	second := f.snapshotRequests()
	for _, r := range second {
		if r.digest == img.layerDigests[0] {
			t.Errorf("layer 1 was re-downloaded although run 1 finished it: %+v", r)
		}
		if r.digest == img.configDig {
			t.Errorf("config was re-downloaded although run 1 finished it")
		}
	}
	if len(second) >= len(firstRun) {
		t.Errorf("resume did not reduce work: %d requests after %d", len(second), len(firstRun))
	}
	got := readArchive(t, out)
	for i, want := range img.layers {
		if !bytes.Equal(got.layers[i], want) {
			t.Fatalf("layer %d wrong after resume", i+1)
		}
	}
}

func TestForceIgnoresCache(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{150000}, "amd64")
	f.addImage(t, "v1", img)

	o := baseOptions(t, f)
	o.Output = filepath.Join(t.TempDir(), "img.tar")
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	f.resetCounters()
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("cached pull: %v", err)
	}
	if got := len(f.snapshotRequests()); got != 0 {
		t.Fatalf("cached pull hit the network %d times", got)
	}

	f.resetCounters()
	o.Force = true
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("forced pull: %v", err)
	}
	if got := len(f.snapshotRequests()); got == 0 {
		t.Error("--force must re-download instead of trusting the cache")
	}
}

func TestCorruptBlobIsRejected(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{150000}, "amd64")
	f.addImage(t, "v1", img)
	f.mu.Lock()
	f.badData[img.layerDigests[0]] = bytes.Repeat([]byte("x"), len(img.layers[0]))
	f.mu.Unlock()

	o := baseOptions(t, f)
	o.Retries = 1
	out := filepath.Join(t.TempDir(), "img.tar")
	o.Output = out
	if _, err := app.Pull(context.Background(), o); err == nil {
		t.Fatal("corrupted data must fail verification")
	} else if !strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "校验") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("archive written despite verification failure")
	}
}

func TestEndpointWithoutRangeSupportStillWorks(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{260000, 80000}, "amd64")
	f.addImage(t, "v1", img)
	f.acceptRanges = false

	o := baseOptions(t, f)
	out := filepath.Join(t.TempDir(), "img.tar")
	o.Output = out
	o.KeepArchive = true
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("pull from a non-ranged mirror: %v", err)
	}
	got := readArchive(t, out)
	for i, want := range img.layers {
		if !bytes.Equal(got.layers[i], want) {
			t.Fatalf("layer %d corrupted when Range is unsupported", i+1)
		}
	}
	for _, r := range f.snapshotRequests() {
		if r.start != 0 {
			t.Fatalf("requested bytes=%d- from a server that ignores Range", r.start)
		}
	}
}

// Some mirrors advertise Accept-Ranges but answer every Range request with the
// whole blob. Chunked downloads would corrupt the layer, so dpull must detect
// this and fall back to one sequential stream.
func TestLyingRangeEndpointFallsBackToSingleStream(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{220000, 70000}, "amd64")
	f.addImage(t, "v1", img)
	f.mu.Lock()
	f.lieAboutRanges = true
	f.mu.Unlock()

	o := baseOptions(t, f)
	out := filepath.Join(t.TempDir(), "img.tar")
	o.Output = out
	o.KeepArchive = true
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("pull from a lying mirror: %v", err)
	}
	got := readArchive(t, out)
	for i, want := range img.layers {
		if !bytes.Equal(got.layers[i], want) {
			t.Fatalf("layer %d corrupted (got %d bytes, want %d)", i+1, len(got.layers[i]), len(want))
		}
	}
}

// Azure/MCR answers ranged GETs correctly but omits Accept-Ranges on HEAD.
// Trusting that header would silently drop the whole image to one connection
// per layer, which is exactly the "slow pull" complaint this tool exists for.
func TestRangeSupportIsProbedNotAssumed(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{300000, 120000}, "amd64")
	f.addImage(t, "v1", img)
	f.mu.Lock()
	f.advertiseRange = false // HEAD says nothing about ranges
	f.mu.Unlock()

	o := baseOptions(t, f)
	out := filepath.Join(t.TempDir(), "img.tar")
	o.Output = out
	o.KeepArchive = true
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("pull: %v", err)
	}
	var midRange int
	for _, r := range f.snapshotRequests() {
		if r.start > 0 {
			midRange++
		}
	}
	if midRange == 0 {
		t.Error("server was never asked for a middle chunk: it was assumed to lack range support")
	}
	got := readArchive(t, out)
	for i, want := range img.layers {
		if !bytes.Equal(got.layers[i], want) {
			t.Fatalf("layer %d wrong", i+1)
		}
	}
}

func TestPushAfterPull(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{120000, 60000}, "amd64")
	f.addImage(t, "v1", img)

	o := baseOptions(t, f)
	o.Output = filepath.Join(t.TempDir(), "img.tar")
	o.PushTargets = []string{f.Host() + "/ns/dst:v2"}
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("pull+push: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.pushBlobs[img.configDig], img.config) {
		t.Error("pushed config differs")
	}
	for i, gz := range img.layers {
		if !bytes.Equal(f.pushBlobs[img.layerDigests[i]], gz) {
			t.Errorf("pushed layer %d differs", i+1)
		}
	}
	if !bytes.Equal(f.pushManifests["v2"], img.manifest) {
		t.Error("pushed manifest differs")
	}
}

func TestOCILayoutOutput(t *testing.T) {
	f := newFakeRegistry(t)
	img := makeImage(t, []int{140000}, "amd64")
	f.addImage(t, "v1", img)

	dir := filepath.Join(t.TempDir(), "img.oci")
	o := baseOptions(t, f)
	o.Format = "oci"
	o.Output = dir
	if _, err := app.Pull(context.Background(), o); err != nil {
		t.Fatalf("oci pull: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "oci-layout")); err != nil || !bytes.Contains(b, []byte("1.0.0")) {
		t.Fatalf("oci-layout missing: %v %s", err, b)
	}
	idxRaw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Manifests []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 || idx.Manifests[0].Digest != img.manifestDig {
		t.Fatalf("index.json = %s", idxRaw)
	}
	for _, d := range append([]string{img.configDig}, img.layerDigests...) {
		p := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:"))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing blob %s: %v", d, err)
		}
		if digestOf(b) != d {
			t.Errorf("blob %s content mismatch", d)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(img.manifestDig, "sha256:"))); err != nil || !bytes.Equal(b, img.manifest) {
		t.Errorf("manifest blob missing or wrong: %v", err)
	}
}

func TestManifestListPicksRequestedPlatform(t *testing.T) {
	f := newFakeRegistry(t)
	amd := makeImage(t, []int{60000}, "amd64")
	arm := makeImage(t, []int{120000}, "arm64")
	f.mu.Lock()
	f.blobs[amd.configDig] = amd.config
	for i, gz := range amd.layers {
		f.blobs[amd.layerDigests[i]] = gz
	}
	f.blobs[arm.configDig] = arm.config
	for i, gz := range arm.layers {
		f.blobs[arm.layerDigests[i]] = gz
	}
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.list.v2+json",
		"manifests": []any{
			map[string]any{"mediaType": amd.manifestMT, "size": len(amd.manifest), "digest": amd.manifestDig,
				"platform": map[string]string{"architecture": "amd64", "os": "linux"}},
			map[string]any{"mediaType": arm.manifestMT, "size": len(arm.manifest), "digest": arm.manifestDig,
				"platform": map[string]string{"architecture": "arm64", "os": "linux", "variant": "v8"}},
		},
	}
	body, _ := json.Marshal(index)
	f.manifests["v1"] = manifestRec{body: body, digest: digestOf(body),
		mediaType: "application/vnd.docker.distribution.manifest.list.v2+json"}
	f.manifests[amd.manifestDig] = manifestRec{body: amd.manifest, digest: amd.manifestDig, mediaType: amd.manifestMT}
	f.manifests[arm.manifestDig] = manifestRec{body: arm.manifest, digest: arm.manifestDig, mediaType: arm.manifestMT}
	f.mu.Unlock()

	out := filepath.Join(t.TempDir(), "img.tar")
	o := baseOptions(t, f)
	o.Output = out
	o.KeepArchive = true
	o.Platform = "linux/amd64"
	results, err := app.Pull(context.Background(), o)
	if err != nil {
		t.Fatalf("pull by digest tag: %v", err)
	}
	if results[0].Bytes != amd.totalSize() {
		t.Errorf("downloaded %d bytes, want amd64 image of %d", results[0].Bytes, amd.totalSize())
	}
	got := readArchive(t, out)
	if !bytes.Equal(got.layers[0], amd.layers[0]) {
		t.Error("wrong platform layer selected")
	}
	if f.manifestGetCount(amd.manifestDig) == 0 {
		t.Error("platform manifest was not fetched")
	}
}

// ---- helpers ----

func (f *fakeRegistry) manifestGetCount(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, g := range f.manifestGets {
		if g == ref {
			n++
		}
	}
	return n
}

func (img *testImage) totalSize() int64 {
	n := int64(len(img.config))
	for _, gz := range img.layers {
		n += int64(len(gz))
	}
	return n
}

type archiveContent struct {
	layers       [][]byte
	config       []byte
	rawManifest  []byte
	manifestJSON []struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}
	names []string
}

func readArchive(t *testing.T, path string) archiveContent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var out archiveContent
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out.names = append(out.names, hdr.Name)
		files[hdr.Name] = b
	}
	raw, ok := files["manifest.json"]
	if !ok {
		t.Fatalf("manifest.json missing, archive holds %v", out.names)
	}
	if err := json.Unmarshal(raw, &out.manifestJSON); err != nil {
		t.Fatalf("manifest.json: %v (%s)", err, raw)
	}
	if len(out.manifestJSON) != 1 {
		t.Fatalf("manifest.json entries = %d", len(out.manifestJSON))
	}
	m := out.manifestJSON[0]
	out.config = files[m.Config]
	out.rawManifest = files["dpull-manifest.json"]
	for _, name := range m.Layers {
		b, ok := files[name]
		if !ok {
			t.Fatalf("layer %s listed in manifest.json but absent from tar %v", name, out.names)
		}
		out.layers = append(out.layers, b)
	}
	return out
}

func gunzipDigest(t *testing.T, gz []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return digestOf(raw)
}

func hexName(digest string) string { return strings.TrimPrefix(digest, "sha256:") }
