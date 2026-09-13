package app

// Importing an archive into the local Docker engine.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// dockerLoad shells out to the docker CLI.
func dockerLoad(ctx context.Context, bin, path string, verbose bool) error {
	if bin == "" {
		bin = "docker"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("找不到 %s，无法执行 docker load（可用 --output 只导出 tar 文件）", bin)
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "docker load -i %s\n", path)
	}
	cmd := exec.CommandContext(ctx, bin, "load", "-i", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker load 失败: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	if verbose {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(string(out)))
	}
	return nil
}
