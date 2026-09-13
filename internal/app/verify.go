package app

// Consistency checks between the downloaded config blob and the layer list,
// including optional deep diffID verification.

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"dpull/internal/registry"
	"dpull/internal/store"
)

// verifyConfigLayers checks the downloaded config against the layer list. This
// catches mirror/index mismatches before they reach `docker load`.
func verifyConfigLayers(cfg []byte, layers []registry.Descriptor, deep bool, st *store.Store) error {
	var doc struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		RootFS       struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(cfg, &doc); err != nil {
		return fmt.Errorf("镜像配置解析失败: %w", err)
	}
	if doc.RootFS.Type != "" && doc.RootFS.Type != "layers" {
		return fmt.Errorf("不支持的 rootfs 类型 %q", doc.RootFS.Type)
	}
	if len(doc.RootFS.DiffIDs) != len(layers) {
		return fmt.Errorf("层数不一致: 配置 %d, manifest %d（镜像源数据可能有误，可加 --force 重拉）",
			len(doc.RootFS.DiffIDs), len(layers))
	}
	if !deep {
		return nil
	}
	for i, want := range doc.RootFS.DiffIDs {
		got, err := diffIDOf(st.BlobPath(layers[i].Digest))
		if err != nil {
			return fmt.Errorf("layer %d 校验失败: %w", i+1, err)
		}
		if got != want {
			return fmt.Errorf("layer %d diffID 不符: 期望 %s, 实际 %s", i+1, want, got)
		}
	}
	return nil
}

// diffIDOf hashes the *uncompressed* layer stream, the way docker does.
func diffIDOf(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var r io.Reader = f
	mag := make([]byte, 2)
	if _, err := f.Read(mag); err == nil {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		switch {
		case mag[0] == 0x1f && mag[1] == 0x8b:
			gz, err := gzip.NewReader(f)
			if err != nil {
				return "", err
			}
			defer gz.Close()
			r = gz
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
