// Package archive turns verified registry blobs into loadable image archives:
// the docker-archive format consumed by `docker load` and the OCI layout
// consumed by podman/skopeo/ctr.
package archive

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bufioOf wraps w in a 1 MiB buffered writer.
func bufioOf(w io.Writer) *bufio.Writer { return bufio.NewWriterSize(w, 1<<20) }

// Blob is one downloaded object referenced by an image.
type Blob struct {
	Digest string // "sha256:..."
	Path   string // file on disk
	Size   int64
	Media  string // media type (OCI layout needs it inside blobs, not here)
}

// Image is everything needed to serialise one platform image.
type Image struct {
	RepoTags   []string
	Config     Blob
	Layers     []Blob // lowest layer first
	Manifest   []byte // raw registry manifest
	ManifestMT string
}

// hexOf strips the algorithm prefix from a digest.
func hexOf(digest string) (string, error) {
	i := strings.Index(digest, ":")
	if i < 0 {
		return "", fmt.Errorf("malformed digest %q", digest)
	}
	return digest[i+1:], nil
}

type manifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// WriteDockerArchive writes the `docker load` compatible tarball at path.
// Registry blobs are stored as-is (gzip stays gzip): docker decompresses
// transparently while loading, so no re-compression is needed.
func WriteDockerArchive(path string, img Image, progress func(n int64)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufioOf(f)
	tw := tar.NewWriter(bw)
	defer func() {
		tw.Close()
		bw.Flush()
	}()

	confName, err := putFile(tw, hexOfName(img.Config.Digest, ".json"), img.Config.Path, progress)
	if err != nil {
		return err
	}
	layerNames := make([]string, 0, len(img.Layers))
	for _, l := range img.Layers {
		n, err := putFile(tw, hexOfName(l.Digest, ".tar"), l.Path, progress)
		if err != nil {
			return err
		}
		layerNames = append(layerNames, n)
	}

	entries := []manifestEntry{{Config: confName, RepoTags: img.RepoTags, Layers: layerNames}}
	mj, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := putBytes(tw, "manifest.json", mj, progress); err != nil {
		return err
	}
	if err := putBytes(tw, "dpull-manifest.json", img.Manifest, progress); err != nil {
		return err
	}
	// Legacy index, ignored by modern Docker but handy for old tooling.
	if len(img.RepoTags) > 0 && len(layerNames) > 0 {
		top := strings.TrimSuffix(layerNames[len(layerNames)-1], ".tar")
		repo := map[string]map[string]string{}
		for _, t := range img.RepoTags {
			name, tag := splitRef(t)
			if repo[name] == nil {
				repo[name] = map[string]string{}
			}
			repo[name][tag] = top
		}
		b, _ := json.Marshal(repo)
		if err := putBytes(tw, "repositories", b, progress); err != nil {
			return err
		}
	}
	return nil
}

// WriteOCILayout writes an OCI image layout directory.
func WriteOCILayout(dir string, img Image, progress func(n int64)) error {
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return err
	}
	for _, b := range append([]Blob{img.Config}, img.Layers...) {
		if err := linkBlob(b, dir, progress); err != nil {
			return err
		}
	}
	mh := sha256.Sum256(img.Manifest)
	md := "sha256:" + hex.EncodeToString(mh[:])
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", hex.EncodeToString(mh[:])), img.Manifest, 0o644); err != nil {
		return err
	}
	if progress != nil {
		progress(int64(len(img.Manifest)))
	}
	mdesc := map[string]any{
		"mediaType": img.ManifestMT,
		"digest":    md,
		"size":      len(img.Manifest),
	}
	if len(img.RepoTags) > 0 {
		mdesc["annotations"] = map[string]string{
			"org.opencontainers.image.ref.name": img.RepoTags[0],
		}
	}
	idx := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []any{mdesc},
	}
	b, _ := json.MarshalIndent(idx, "", "  ")
	return os.WriteFile(filepath.Join(dir, "index.json"), b, 0o644)
}

func linkBlob(b Blob, dir string, progress func(n int64)) error {
	hexStr, err := hexOf(b.Digest)
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, "blobs", "sha256", hexStr)
	if _, err := os.Stat(dst); err == nil && sizeOf(dst) == b.Size {
		if progress != nil && b.Size > 0 {
			progress(b.Size)
		}
		return nil
	}
	_ = os.Remove(dst)
	if err := os.Link(b.Path, dst); err != nil {
		// different filesystem: fall back to copying
		if err := copyFile(b.Path, dst, progress); err != nil {
			return err
		}
		return nil
	}
	if progress != nil && b.Size > 0 {
		progress(b.Size)
	}
	return nil
}

func sizeOf(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return st.Size()
}

func copyFile(src, dst string, progress func(n int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	var w io.Writer = out
	if progress != nil {
		w = &fnWriter{w: out, fn: progress}
	}
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(w, in, buf); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

type fnWriter struct {
	w  io.Writer
	fn func(int64)
}

func (f *fnWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if n > 0 {
		f.fn(int64(n))
	}
	return n, err
}

func hexOfName(digest, suffix string) string {
	h, err := hexOf(digest)
	if err != nil {
		h = "unknown"
	}
	return h + suffix
}

func putFile(tw *tar.Writer, name, path string, progress func(int64)) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	hdr := &tar.Header{
		Name:    name,
		Size:    st.Size(),
		Mode:    0o420,
		ModTime: time.Unix(0, 0),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var r io.Reader = f
	if progress != nil {
		r = &fnWriter2{r: f, fn: progress}
	}
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(tw, r, buf); err != nil {
		return "", err
	}
	return name, nil
}

type fnWriter2 struct {
	r  io.Reader
	fn func(int64)
}

func (f *fnWriter2) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 {
		f.fn(int64(n))
	}
	return n, err
}

func putBytes(tw *tar.Writer, name string, b []byte, progress func(int64)) error {
	if len(b) == 0 {
		return nil
	}
	hdr := &tar.Header{Name: name, Size: int64(len(b)), Mode: 0o420, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	if progress != nil {
		progress(int64(len(b)))
	}
	return nil
}

func splitRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}
