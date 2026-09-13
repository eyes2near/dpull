package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
)

// Media types we understand.
const (
	MediaManifestV2      = "application/vnd.docker.distribution.manifest.v2+json"
	MediaManifestListV2  = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	MediaOCIIndex        = "application/vnd.oci.image.index.v1+json"
	MediaOCIManifestZstd = "application/vnd.oci.image.manifest.v1+json+zstd"
	MediaDockerConfig    = "application/vnd.docker.container.image.v1+json"
	MediaOCILayer        = "application/vnd.oci.image.layer.v1.tar"
	MediaOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaOCILayerZstd    = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaOCILayerNondist = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"
)

// Descriptor is a content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Layers      []Descriptor      `json:"layers,omitempty"` // only used by image configs
	Config      *Descriptor       `json:"config,omitempty"`
}

// Platform describes an image target platform.
type Platform struct {
	OS           string   `json:"os"`
	Architecture string   `json:"architecture"`
	Variant      string   `json:"variant,omitempty"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// ManifestV2 is a single-platform image manifest.
type ManifestV2 struct {
	MediaType     string       `json:"mediaType"`
	SchemaVersion int          `json:"schemaVersion"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

type imageIndex struct {
	MediaType   string            `json:"mediaType"`
	Manifests   []Descriptor      `json:"manifests"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Resolved is the outcome of resolving an image reference to a platform.
type Resolved struct {
	Ref            string // what the user typed
	Repo           string
	Platform       Platform
	MediaType      string
	ManifestBytes  []byte // manifest to re-upload when pushing
	Config         Descriptor
	Layers         []Descriptor // download order, lowest layer first
	TotalBytes     int64
	IndexDigest    string // digest of the tag manifest (may equal manifest digest)
	ManifestDigest string
	Endpoint       Endpoint // where it was resolved from
}

// AcceptManifests is the Accept header used for manifest negotiation.
var AcceptManifests = strings.Join([]string{
	MediaManifestV2,
	MediaManifestListV2,
	MediaOCIManifest,
	MediaOCIIndex,
	"application/vnd.docker.distribution.manifest.v1+json",
}, ", ")

// fetchManifest returns raw manifest bytes plus the digest header.
func (c *Client) fetchManifest(ctx context.Context, repo, ref string) (body []byte, mediaType, digest string, ep Endpoint, err error) {
	path := "/v2/" + escapePath(repo) + "/manifests/" + urlEscapeRef(ref)
	res, err := c.Do(ctx, Request{
		Method:   "GET",
		Repo:     repo,
		Action:   "pull",
		Path:     path,
		Failover: true,
		Headers:  header("Accept", AcceptManifests),
	})
	if err != nil {
		return nil, "", "", Endpoint{}, err
	}
	defer res.Close()
	body, err = io.ReadAll(res.Resp.Body)
	if err != nil {
		return nil, "", "", Endpoint{}, fmt.Errorf("read manifest from %s: %w", res.Endpoint.Name, err)
	}
	mt := res.Resp.Header.Get("Content-Type")
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	dg := firstNonEmpty(res.Resp.Header.Get("Docker-Content-Digest"), "")
	return body, mt, dg, res.Endpoint, nil
}

// Resolve fetches the manifest for ref and picks the requested platform.
func (c *Client) Resolve(ctx context.Context, repo, ref, wantPlatform string) (*Resolved, error) {
	body, mt, digest, ep, err := c.fetchManifest(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	out := &Resolved{Repo: repo, IndexDigest: digest, Endpoint: ep}

	if isIndex(mt, body) {
		var idx imageIndex
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, fmt.Errorf("parse index: %w", err)
		}
		want := parsePlatform(wantPlatform)
		desc, err := pickPlatform(idx.Manifests, want)
		if err != nil {
			return nil, err
		}
		if desc.Platform != nil {
			out.Platform = *desc.Platform
		}
		out.IndexDigest = firstNonEmpty(digest, desc.Digest)
		// The platform manifest must come from the same endpoint to keep
		// mirror behaviour predictable.
		body2, mt2, dg2, _, err := c.fetchManifest(ctx, repo, desc.Digest)
		if err != nil {
			return nil, err
		}
		body, mt, digest = body2, mt2, dg2
	}

	var m ManifestV2
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Config.Digest == "" || len(m.Layers) == 0 {
		// schema1 or something exotic
		return nil, fmt.Errorf("unsupported manifest (%s): no config/layers; try a v2 tagged image", mt)
	}
	if mt == "" {
		mt = m.MediaType
	}
	if out.Platform.OS == "" {
		out.Platform = c.guessPlatform(ctx, m.Config)
	}
	m.MediaType = mt
	out.MediaType = mt
	out.Config = m.Config
	out.Layers = m.Layers
	out.ManifestDigest = firstNonEmpty(digest, out.ManifestDigest)
	out.ManifestBytes = body
	out.TotalBytes = m.Config.Size
	for _, l := range m.Layers {
		out.TotalBytes += l.Size
	}
	return out, nil
}

func isIndex(mt string, body []byte) bool {
	if mt == MediaManifestListV2 || mt == MediaOCIIndex {
		return true
	}
	var probe struct {
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && len(probe.Manifests) > 0 {
		return true
	}
	return false
}

func parsePlatform(s string) Platform {
	if s == "" {
		p := Platform{OS: "linux", Architecture: runtime.GOARCH}
		if runtime.GOARCH == "arm64" {
			p.Variant = "v8"
		} else if runtime.GOARCH == "arm" {
			p.Variant = "v7"
		}
		return p
	}
	parts := strings.Split(s, "/")
	p := Platform{}
	switch len(parts) {
	case 1:
		p.OS = parts[0]
	case 2:
		p.OS, p.Architecture = parts[0], parts[1]
	default:
		p.OS, p.Architecture, p.Variant = parts[0], parts[1], parts[2]
	}
	return p
}

func platformMatch(got, want Platform) bool {
	if want.OS != "" && !strings.EqualFold(got.OS, want.OS) {
		return false
	}
	if want.Architecture != "" && !strings.EqualFold(got.Architecture, want.Architecture) {
		return false
	}
	if want.Variant != "" && got.Variant != "" && !strings.EqualFold(got.Variant, want.Variant) {
		return false
	}
	return true
}

func pickPlatform(cands []Descriptor, want Platform) (Descriptor, error) {
	if len(cands) == 0 {
		return Descriptor{}, fmt.Errorf("manifest list is empty")
	}
	var fallback []Descriptor
	for _, d := range cands {
		if d.Platform == nil {
			fallback = append(fallback, d)
			continue
		}
		if platformMatch(*d.Platform, want) {
			return d, nil
		}
		fallback = append(fallback, d)
	}
	// relax the variant requirement
	want.Variant = ""
	for _, d := range fallback {
		if d.Platform != nil && platformMatch(*d.Platform, want) {
			return d, nil
		}
	}
	return fallback[0], nil
}

// guessPlatform reads os/arch out of the image config for display purposes.
func (c *Client) guessPlatform(ctx context.Context, cfg Descriptor) Platform {
	return Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// ConfigOSArch reads os/architecture from a config blob.
func ConfigOSArch(b []byte) Platform {
	var p struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	_ = json.Unmarshal(b, &p)
	return Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant}
}

func urlEscapeRef(ref string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case strings.ContainsRune("-._~:@", r):
			return r
		}
		return '_'
	}, ref)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isGzipLayer(d Descriptor) bool {
	switch d.MediaType {
	case MediaDockerLayerGzip, MediaOCILayerGzip, MediaOCILayerNondist:
		return true
	}
	return false
}
