package reference

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		registry string
		repo     string
		tag      string
		digest   string
		local    string
	}{
		{in: "nginx", registry: "docker.io", repo: "library/nginx", tag: "latest", local: "nginx:latest"},
		{in: "nginx:1.27", registry: "docker.io", repo: "library/nginx", tag: "1.27", local: "nginx:1.27"},
		{in: "docker.io/nginx:1.27", registry: "docker.io", repo: "library/nginx", tag: "1.27", local: "nginx:1.27"},
		{in: "myuser/myapp:v1.2", registry: "docker.io", repo: "myuser/myapp", tag: "v1.2", local: "myuser/myapp:v1.2"},
		{in: "registry.k8s.io/pause:3.10", registry: "registry.k8s.io", repo: "pause", tag: "3.10", local: "pause:3.10"},
		{in: "localhost:5000/app:dev", registry: "localhost:5000", repo: "app", tag: "dev", local: "app:dev"},
		{in: "127.0.0.1:5000/ns/app", registry: "127.0.0.1:5000", repo: "ns/app", tag: "latest", local: "ns/app:latest"},
		{
			in: "alpine@sha256:d9e853e87e55", registry: "docker.io", repo: "library/alpine",
			digest: "sha256:d9e853e87e55", local: "alpine@sha256:d9e853e87e55",
		},
		{in: "registry.cn-hangzhou.aliyuncs.com/me/app:v1", registry: "registry.cn-hangzhou.aliyuncs.com",
			repo: "me/app", tag: "v1", local: "me/app:v1"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got.Registry != c.registry || got.Repository != c.repo || got.Tag != c.tag || got.Digest != c.digest {
			t.Errorf("Parse(%q) = %+v, want registry=%s repo=%s tag=%s digest=%s",
				c.in, got, c.registry, c.repo, c.tag, c.digest)
		}
		if got.Local() != c.local {
			t.Errorf("Parse(%q).Local() = %q, want %q", c.in, got.Local(), c.local)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "  ", ":latest", "a/b:", "repo@nodigest"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestDockerHubHost(t *testing.T) {
	r, err := Parse("nginx:1.27")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsDockerHub() || r.Host() != DockerHubHost {
		t.Errorf("Host() = %q, want %q", r.Host(), DockerHubHost)
	}
	if r.String() != DockerHubHost+"/library/nginx:1.27" {
		t.Errorf("String() = %q", r.String())
	}
}

func TestShortName(t *testing.T) {
	r, _ := Parse("registry.k8s.io/sig-storage/csi-provisioner:v4.0.0")
	if got := r.ShortName(); got != "csi-provisioner_v4.0.0" {
		t.Errorf("ShortName = %q", got)
	}
	d, _ := Parse("alpine@sha256:aaaa")
	if got := d.ShortName(); got != "alpine_sha256-aaaa" {
		t.Errorf("digest ShortName = %q", got)
	}
}

func TestParseTargetRequiresTag(t *testing.T) {
	if _, err := ParseTarget("reg.example.com/a/b@sha256:aa"); err == nil {
		t.Error("push targets must not use digests")
	}
	r, err := ParseTarget("reg.example.com/a/b:v2")
	if err != nil || r.Repository != "a/b" || r.Tag != "v2" {
		t.Errorf("ParseTarget = %+v, %v", r, err)
	}
}
