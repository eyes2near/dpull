package app

// Re-uploading a pulled image (config, layers, manifest) to another registry.

import (
	"context"
	"fmt"
	"io"
	"os"

	"dpull/internal/reference"
	"dpull/internal/registry"
	"dpull/internal/store"
	"dpull/internal/xfer"
)

// pushImage uploads config + layers + manifest to a target registry.
func pushImage(ctx context.Context, o *Options, st *store.Store, res *registry.Resolved, target reference.Ref, prog *xfer.Progress) error {
	ep, err := parseEndpoint("target:"+target.Host(), target.Host(), o.Insecure, plainHTTPHosts(o))
	if err != nil {
		return err
	}
	cli := newClient(o, []registry.Endpoint{ep})
	// keep transport settings but allow a bigger header timeout for pushes
	chunk := int64(16 << 20)
	if o.ChunkSize > chunk {
		chunk = o.ChunkSize
	}
	pushItem := &xfer.Item{Label: "推送 " + target.ShortName(), Size: res.TotalBytes}
	pushItem.SetState(xfer.StateActive)
	prog.StartWith([]*xfer.Item{pushItem}, "上传")
	defer func() { prog.Stop(nil) }()

	pushOne := func(dig string, size int64) error {
		path := st.BlobPath(dig)
		return cli.PushBlob(ctx, registry.PutBlobOptions{
			Repo:     target.Repository,
			Digest:   dig,
			Size:     size,
			Chunk:    chunk,
			Progress: func(n int64) { pushItem.Done.Add(n) },
			Open: func() (io.ReadCloser, error) {
				f, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				st, err := f.Stat()
				if err == nil && size > 0 && st.Size() != size {
					f.Close()
					return nil, fmt.Errorf("%s 大小不符: 磁盘 %d, manifest %d", dig, st.Size(), size)
				}
				return f, nil
			},
		})
	}
	if err := pushOne(res.Config.Digest, res.Config.Size); err != nil {
		return err
	}
	for i, l := range res.Layers {
		if err := pushOne(l.Digest, l.Size); err != nil {
			return fmt.Errorf("layer %d: %w", i+1, err)
		}
	}
	tag := target.Tag
	if tag == "" {
		tag = "latest"
	}
	if err := cli.PutManifest(ctx, target.Repository, tag, res.ManifestBytes, res.MediaType); err != nil {
		pushItem.SetState(xfer.StateFailed)
		return err
	}
	pushItem.SetState(xfer.StateDone)
	return nil
}
