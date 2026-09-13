// Package reference parses Docker image references such as
// "nginx:1.25", "docker.io/library/nginx:1.25",
// "registry.cn-hangzhou.aliyuncs.com/ns/app:v1" or "busybox@sha256:...".
package reference

import (
	"fmt"
	"strings"
)

const (
	// DefaultRegistry is the canonical host of Docker Hub.
	DefaultRegistry = "docker.io"
	// DockerHubHost is the real registry endpoint behind docker.io.
	DockerHubHost = "registry-1.docker.io"
	DefaultTag    = "latest"
)

// Ref is a parsed image reference.
type Ref struct {
	Registry   string // registry host, "docker.io" normalised to "registry-1.docker.io"
	Repository string // path within the registry, e.g. "library/nginx"
	Tag        string // empty when Digest is set
	Digest     string // "sha256:..." or empty
}

// String renders the reference back into its canonical form.
func (r Ref) String() string {
	s := r.Host() + "/" + r.Repository
	if r.Digest != "" {
		return s + "@" + r.Digest
	}
	return s + ":" + r.Tag
}

// Host returns the registry host to talk to.
func (r Ref) Host() string {
	if r.Registry == DefaultRegistry {
		return DockerHubHost
	}
	return r.Registry
}

// IsDockerHub reports whether this reference points at Docker Hub.
func (r Ref) IsDockerHub() bool { return r.Registry == DefaultRegistry }

// Local renders the reference without a registry host, the way docker load
// names images: "library/nginx:1.25" or "my/app@sha256:...".
func (r Ref) Local() string {
	repo := r.Repository
	if r.IsDockerHub() {
		repo = strings.TrimPrefix(repo, "library/")
	}
	if r.Digest != "" {
		return repo + "@" + r.Digest
	}
	return repo + ":" + r.Tag
}

// ForLoad renders the reference the way `docker load` should name the image:
// Docker Hub images keep docker's familiar short form ("nginx:1.27") while
// images from other registries keep their host, exactly like `docker pull`
// lists them.
func (r Ref) ForLoad(tagOverride string) string {
	tag := tagOverride
	if tag == "" {
		if r.Digest != "" {
			short := strings.ReplaceAll(r.Digest, ":", "-")
			if len(short) > 19 {
				short = short[:19]
			}
			tag = short
		} else {
			tag = r.Tag
		}
	}
	if r.IsDockerHub() {
		return strings.TrimPrefix(r.Repository, "library/") + ":" + tag
	}
	name := r.Host() + "/" + r.Repository
	if r.Digest != "" && tagOverride == "" {
		return name + "@" + r.Digest + ":" + tag
	}
	return name + ":" + tag
}

// ShortName returns a filesystem friendly name, e.g. "nginx_1.25".
func (r Ref) ShortName() string {
	last := r.Repository
	if i := strings.LastIndex(last, "/"); i >= 0 {
		last = last[i+1:]
	}
	tag := r.Tag
	if tag == "" {
		tag = strings.ReplaceAll(r.Digest, ":", "-")
	}
	return sanitise(last + "_" + tag)
}

func sanitise(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Parse parses an image reference. An empty tag defaults to "latest".
func Parse(s string) (Ref, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Ref{}, fmt.Errorf("empty image reference")
	}
	var out Ref
	// digest
	if i := strings.Index(raw, "@"); i >= 0 {
		out.Digest = raw[i+1:]
		raw = raw[:i]
		if !strings.Contains(out.Digest, ":") {
			return Ref{}, fmt.Errorf("invalid digest %q", out.Digest)
		}
	}
	// tag: a colon that appears after the last '/' is a tag separator
	if out.Digest == "" {
		if i := strings.LastIndex(raw, ":"); i >= 0 && strings.LastIndex(raw, "/") < i {
			out.Tag = raw[i+1:]
			if out.Tag == "" {
				return Ref{}, fmt.Errorf("empty tag in %q", s)
			}
			raw = raw[:i]
		}
	}
	if raw == "" {
		return Ref{}, fmt.Errorf("missing repository in %q", s)
	}
	// registry
	first := raw
	if i := strings.Index(raw, "/"); i >= 0 {
		first = raw[:i]
	}
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		out.Registry = first
		out.Repository = raw[len(first)+1:]
	} else {
		out.Registry = DefaultRegistry
		out.Repository = raw
	}
	if out.Repository == "" {
		return Ref{}, fmt.Errorf("missing repository in %q", s)
	}
	// Docker Hub official images live under library/.
	if out.IsDockerHub() && !strings.Contains(out.Repository, "/") {
		out.Repository = "library/" + out.Repository
	}
	if out.Tag == "" && out.Digest == "" {
		out.Tag = DefaultTag
	}
	if out.Tag != "" && !validTag(out.Tag) {
		return Ref{}, fmt.Errorf("invalid tag %q", out.Tag)
	}
	return out, nil
}

// ParseTarget is like Parse but keeps an explicit registry host as-is and
// requires a tag (used for --push targets).
func ParseTarget(s string) (Ref, error) {
	r, err := Parse(s)
	if err != nil {
		return Ref{}, err
	}
	if r.Digest != "" {
		return Ref{}, fmt.Errorf("push target %q must use a tag, not a digest", s)
	}
	return r, nil
}

func validTag(t string) bool {
	if t == "" || len(t) > 128 {
		return false
	}
	for i, c := range t {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.' || c == '+':
		default:
			return false
		}
		_ = i
	}
	return true
}
