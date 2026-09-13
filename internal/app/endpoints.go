package app

// Endpoint selection: mirrors from flags/env/docker daemon config, in priority order,
// followed by the origin registry, plus the HTTP client construction used
// everywhere else.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"dpull/internal/reference"
	"dpull/internal/registry"
)

// endpointsFor builds the mirror-first endpoint list for a reference.
func endpointsFor(o *Options, r reference.Ref) ([]registry.Endpoint, error) {
	var eps []registry.Endpoint
	plain := plainHTTPHosts(o)
	seen := map[string]bool{}
	add := func(name, raw string) error {
		ep, err := parseEndpoint(name, raw, o.Insecure, plain)
		if err != nil {
			return err
		}
		if seen[ep.Base] {
			return nil
		}
		seen[ep.Base] = true
		eps = append(eps, ep)
		return nil
	}
	for i, m := range o.Mirrors {
		if err := add(fmt.Sprintf("mirror%d", i+1), m); err != nil {
			return nil, err
		}
	}
	if r.IsDockerHub() {
		for i, m := range dockerDaemonMirrors() {
			if err := add(fmt.Sprintf("daemon-mirror%d", i+1), m); err == nil {
				seen[m] = true
			}
		}
	}
	if err := add("origin:"+r.Host(), r.Host()); err != nil {
		return nil, err
	}
	return eps, nil
}

// plainHTTPHosts lists hosts that may be talked to over http: the ones the user
// named with --plain-http plus, when --insecure is set, loopback addresses only.
// Downgrading *every* endpoint to http would break public registries, which
// answer with an HTML redirect page that then fails to parse as a manifest.
func plainHTTPHosts(o *Options) map[string]bool {
	out := map[string]bool{}
	for _, h := range o.PlainHTTP {
		h = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "http://"), "https://"))
		if h != "" {
			out[strings.TrimSuffix(h, "/")] = true
		}
	}
	return out
}

func parseEndpoint(name, raw string, allowInsecure bool, plainHosts map[string]bool) (registry.Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return registry.Endpoint{}, errors.New("空的 registry 地址")
	}
	host := raw
	// http is opt-in per host: an explicit http:// prefix, --plain-http, or
	// --insecure for loopback addresses. Applying it to every endpoint would
	// send public registries over http and break manifest parsing.
	insecure := false
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return registry.Endpoint{}, fmt.Errorf("无效的 registry 地址 %q: %w", raw, err)
		}
		host = u.Host
		if u.Host == "" {
			host = u.Path
		}
		if u.Scheme == "http" {
			insecure = true
		}
	} else {
		host = strings.TrimPrefix(raw, "registry://")
	}
	host = strings.TrimSuffix(host, "/")
	if !insecure && plainHosts[host] {
		insecure = true
	}
	if !insecure && allowInsecure && isLoopbackHost(host) {
		insecure = true
	}
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	return registry.Endpoint{
		Name:     name,
		Base:     scheme + "://" + host,
		Host:     host,
		Insecure: insecure,
	}, nil
}

func newClient(o *Options, eps []registry.Endpoint) *registry.Client {
	note := func(format string, args ...any) {
		if !o.Quiet {
			fmt.Fprintf(os.Stderr, "  · "+format+"\n", args...)
		}
	}
	cli := registry.New(eps, registry.Config{
		Concurrency:   o.Concurrency,
		SkipTLSVerify: o.SkipTLS,
		PreferIPv4:    o.PreferIPv4,
		HostOverrides: o.Resolves,
		DohServers:    o.DoH,
		DisableDoH:    o.NoDoH,
		Verbose:       os.Getenv("DPULL_DEBUG") != "",
		Note:          note,
	})
	cli.CredentialsFor = credentialsFor(o)
	cli.Logf = note
	return cli
}

// dockerDaemonMirrors reads registry-mirrors from ~/.docker/daemon.json when
// the user did not pass --mirror.
// isLoopbackHost covers localhost, 127.0.0.0/8 and ::1 with or without a port.
func isLoopbackHost(host string) bool {
	name := host
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i+1:], ":") {
		name = host[:i]
	}
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	if ip := net.ParseIP(name); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func dockerDaemonMirrors() []string {
	var out []string
	homes := []string{}
	if dc := os.Getenv("DOCKER_CONFIG"); dc != "" {
		homes = append(homes, filepath.Join(dc, "daemon.json"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		homes = append(homes, filepath.Join(h, ".docker", "daemon.json"))
	}
	for _, p := range homes {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc struct {
			RegistryMirrors []string `json:"registry-mirrors"`
		}
		if err := json.Unmarshal(b, &doc); err == nil {
			out = append(out, doc.RegistryMirrors...)
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}
