package main

import (
	"errors"
	"flag"
	"strings"
	"testing"

	"dpull/internal/app"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"8Mi", 8 << 20},
		{"8mib", 8 << 20},
		{"8M", 8 << 20},
		{"512k", 512 << 10},
		{"1G", 1 << 30},
		{"1.5Mi", 1572864},
		{"4096", 4096},
		{"2MB", 2000000},
		{" 16mi ", 16 << 20},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"0", "-4", "abc", "8Xi"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should fail", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want int64 // nanoseconds
	}{
		{"30s", 30_000_000_000},
		{"30", 30_000_000_000},
		{"1m", 60_000_000_000},
		{"1500ms", 1_500_000_000},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		if err != nil {
			t.Errorf("parseDuration(%q): %v", c.in, err)
			continue
		}
		if int64(got) != c.want {
			t.Errorf("parseDuration(%q) = %v", c.in, got)
		}
	}
	// sub-second values are meaningless for a stall detector
	if _, err := parseDuration("500ms"); err == nil {
		t.Error("500ms should be rejected")
	}
	if _, err := parseDuration("tomorrow"); err == nil {
		t.Error("garbage should be rejected")
	}
}

func TestReorderMovesFlagsFirst(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(discard{})
	var (
		concurrency int
		quiet       bool
		multi       listFlag
	)
	fs.IntVar(&concurrency, "c", 8, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	fs.Var(&multi, "mirror", "")
	fs.StringVar(new(string), "push", "", "")

	in := []string{"nginx:1.27", "-c", "16", "--quiet", "redis:7", "--mirror=a", "--push", "reg/x/y:v1"}
	out := reorder(in, fs)
	if err := fs.Parse(out); err != nil {
		t.Fatalf("parse reordered: %v (%v)", err, out)
	}
	if concurrency != 16 || !quiet {
		t.Errorf("flags lost: %v %v (%v)", concurrency, quiet, out)
	}
	if len(multi) != 1 || multi[0] != "a" {
		t.Errorf("--mirror lost: %v", multi)
	}
	if len(fs.Args()) != 2 || fs.Args()[0] != "nginx:1.27" || fs.Args()[1] != "redis:7" {
		t.Errorf("images lost: %v", fs.Args())
	}
}

func TestBoolLike(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(discard{})
	fs.Bool("on", false, "")
	fs.Int("n", 1, "")
	if !boolLike(fs.Lookup("on")) {
		t.Error("--on should be recognised as a bool flag")
	}
	if boolLike(fs.Lookup("n")) {
		t.Error("--n takes a value")
	}
	if boolLike(nil) {
		t.Error("unknown flags are not bools")
	}
}

func TestAdviceForCommonFailures(t *testing.T) {
	o := &app.Options{Images: []string{"nginx:1.27"}, PreferIPv4: true}
	cases := []struct {
		errText string
		want    string
	}{
		{"dial tcp 1.2.3.4:443: connect: no route to host", "bench"},
		{"HTTP 429: Too Many Requests", "速率限制"},
		{"HTTP 401: authentication required", "--user"},
		{"digest mismatch: sha256:aa", "--force"},
		{"docker load 失败: exit status 1", "Docker 已启动"},
		{"HTTP 404: MANIFEST_UNKNOWN", "tag"},
		{"everything is fine", ""},
	}
	for _, c := range cases {
		box := advice(errors.New(c.errText), o)
		joined := strings.Join(box.lines, "\n")
		if c.want == "" {
			if joined != "" {
				t.Errorf("unexpected advice for %q: %s", c.errText, joined)
			}
			continue
		}
		if !strings.Contains(joined, c.want) {
			t.Errorf("advice for %q should mention %q, got %q", c.errText, c.want, joined)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
