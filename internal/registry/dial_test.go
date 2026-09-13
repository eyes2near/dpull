package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// poisonedHost never resolves through the system resolver, which is the normal
// shape of a hijacked network: the answer you get is a blackhole, the real
// address has to come from an encrypted resolver.
const poisonedHost = "dpull-dialtest.invalid"

func fakeDoHServer(t *testing.T, answer string, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		qtype := r.URL.Query().Get("type")
		*seen = append(*seen, name+"/"+qtype)
		w.Header().Set("Content-Type", "application/dns-json")
		if name != poisonedHost {
			fmt.Fprint(w, `{"Status":3}`)
			return
		}
		if qtype == "A" {
			fmt.Fprintf(w, `{"Status":0,"Answer":[{"name":"%s.","type":1,"TTL":60,"data":"%s"}]}`, name, answer)
			return
		}
		fmt.Fprint(w, `{"Status":0,"Answer":[]}`)
	}))
}

func TestDialerFallsBackToDoHWhenSystemDNSIsPoisoned(t *testing.T) {
	var seenMu sync.Mutex
	var seen []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "registry ok")
	}))
	defer origin.Close()
	port := origin.URL[strings.LastIndex(origin.URL, ":")+1:]

	doh := fakeDoHServer(t, "127.0.0.1", &seen)
	defer doh.Close()

	var notes []string
	var noteMu sync.Mutex
	cfg := Config{
		Concurrency: 2,
		DohServers:  []string{doh.URL},
		Note: func(format string, args ...any) {
			noteMu.Lock()
			defer noteMu.Unlock()
			notes = append(notes, fmt.Sprintf(format, args...))
		},
	}
	ep := Endpoint{Name: "poisoned", Base: "http://" + poisonedHost + ":" + port, Host: poisonedHost}
	cli := New([]Endpoint{ep}, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cli.Do(ctx, Request{Method: http.MethodGet, Path: "/v2/"})
	if err != nil {
		t.Fatalf("request through the DoH-pinned address failed: %v", err)
	}
	defer res.Close()
	if res.Resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.Resp.StatusCode)
	}
	noteMu.Lock()
	joined := strings.Join(notes, "|")
	noteMu.Unlock()
	if !strings.Contains(joined, "DoH") {
		t.Errorf("expected a note about the DoH fallback, got %q", joined)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) == 0 {
		t.Error("the DoH server was never queried")
	}
}

func TestDialerWithoutDoHCannotEscapePoisonedDNS(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()
	port := origin.URL[strings.LastIndex(origin.URL, ":")+1:]

	var seen []string
	doh := fakeDoHServer(t, "127.0.0.1", &seen)
	defer doh.Close()

	cli := New([]Endpoint{{Name: "poisoned", Base: "http://" + poisonedHost + ":" + port, Host: poisonedHost}},
		Config{Concurrency: 1, DisableDoH: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if res, err := cli.Do(ctx, Request{Method: http.MethodGet, Path: "/v2/"}); err == nil {
		res.Close()
		t.Fatal("expected the poisoned name to fail with DoH disabled")
	}
	if len(seen) != 0 {
		t.Error("DoH must not be consulted when disabled")
	}
}

func TestHostOverridesWin(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pinned")
	}))
	defer origin.Close()
	port := origin.URL[strings.LastIndex(origin.URL, ":")+1:]

	cli := New([]Endpoint{{Name: "pin", Base: "http://" + poisonedHost + ":" + port, Host: poisonedHost}},
		Config{Concurrency: 1, HostOverrides: []string{poisonedHost + "=127.0.0.1"}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := cli.Do(ctx, Request{Method: http.MethodGet, Path: "/v2/"})
	if err != nil {
		t.Fatalf("--resolve pinning failed: %v", err)
	}
	defer res.Close()
	if res.Resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.Resp.StatusCode)
	}
}

func TestParseHostOverrides(t *testing.T) {
	o := parseHostOverrides([]string{" a.example.com = 1.2.3.4 ", "bad", "[::1]:5000=::1", ""})
	if len(o) != 2 {
		t.Fatalf("parsed %+v", o)
	}
	if got, ok := o.resolve("a.example.com:443"); !ok || got != "1.2.3.4:443" {
		t.Errorf("resolve = %q %v", got, ok)
	}
	if _, ok := o.resolve("other.example.com:443"); ok {
		t.Error("unrelated host must not be rewritten")
	}
	if _, ok := o.resolve("nocolon"); ok {
		t.Error("an address without a port cannot be rewritten")
	}
}
