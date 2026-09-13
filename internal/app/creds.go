package app

// Credential discovery: flags, environment, ~/.docker/config.json and
// docker-credential-* helpers.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dpull/internal/reference"
	"dpull/internal/registry"
)

// credEntry is a resolved credential set.
type credEntry struct {
	user, pass string
	token      string
}

// credentialsFor builds a credential resolver from env, --user/--password and
// the docker config file (including credential helpers).
func credentialsFor(o *Options) func(host string) (registry.Credentials, bool) {
	var mu sync.Mutex
	cache := map[string]*credEntry{}
	return func(host string) (registry.Credentials, bool) {
		mu.Lock()
		defer mu.Unlock()
		if e, ok := cache[host]; ok {
			if e == nil {
				return registry.Credentials{}, false
			}
			return registry.Credentials{Username: e.user, Password: e.pass, Token: e.token}, true
		}
		put := func(e *credEntry) (registry.Credentials, bool) {
			cache[host] = e
			if e == nil {
				return registry.Credentials{}, false
			}
			return registry.Credentials{Username: e.user, Password: e.pass, Token: e.token}, true
		}
		if o.Username != "" {
			return put(&credEntry{user: o.Username, pass: o.Password})
		}
		if u := os.Getenv("DPULL_USERNAME"); u != "" {
			return put(&credEntry{user: u, pass: os.Getenv("DPULL_PASSWORD")})
		}
		if u := os.Getenv("DOCKER_USERNAME"); u != "" && strings.Contains(host, "docker.io") {
			return put(&credEntry{user: u, pass: os.Getenv("DOCKER_PASSWORD")})
		}
		if e := dockerConfigCreds(host); e != nil {
			return put(e)
		}
		return put(nil)
	}
}

func dockerConfigCreds(host string) *credEntry {
	cfg, err := loadDockerConfig()
	if err != nil {
		return nil
	}
	candidates := []string{host, "https://" + host, "index." + host}
	if host == reference.DockerHubHost {
		candidates = append(candidates, "https://index.docker.io/v1/", "docker.io", "index.docker.io")
	}
	for _, c := range candidates {
		if a, ok := cfg.Auths[c]; ok {
			if a.Auth != "" {
				if b, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
					if i := strings.Index(string(b), ":"); i > 0 {
						return &credEntry{user: string(b[:i]), pass: string(b[i+1:])}
					}
				}
			}
			if a.Username != "" {
				return &credEntry{user: a.Username, pass: a.Password}
			}
			if a.IdentityToken != "" {
				return &credEntry{token: a.IdentityToken}
			}
		}
	}
	if cfg.CredsStore != "" {
		if e := credsHelper(cfg.CredsStore, host); e != nil {
			return e
		}
	}
	return nil
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

type dockerConfig struct {
	Auths      map[string]dockerAuth `json:"auths"`
	CredsStore string                `json:"credsStore"`
}

var dockerConfigOnce sync.Once

var dockerConfigCache dockerConfig

var dockerConfigErr error

func loadDockerConfig() (dockerConfig, error) {
	dockerConfigOnce.Do(func() {
		var paths []string
		if dc := os.Getenv("DOCKER_CONFIG"); dc != "" {
			paths = append(paths, filepath.Join(dc, "config.json"))
		}
		if h, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(h, ".docker", "config.json"))
		}
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(b, &dockerConfigCache); err != nil {
				dockerConfigErr = err
				continue
			}
			return
		}
		dockerConfigErr = errors.New("未找到 docker config.json")
	})
	return dockerConfigCache, dockerConfigErr
}

// credsHelper runs docker-credential-<store> get.
func credsHelper(storeName, host string) *credEntry {
	bin := "docker-credential-" + storeName
	if _, err := exec.LookPath(bin); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "get")
	cmd.Stdin = strings.NewReader(host)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	var res struct {
		Secret   string `json:"Secret"`
		Username string `json:"Username"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil
	}
	if res.Username == "<token>" || (res.Username == "" && res.Secret != "") {
		return &credEntry{token: res.Secret}
	}
	return &credEntry{user: res.Username, pass: res.Secret}
}
