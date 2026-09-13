// Package app wires the pieces together: endpoints, auth, resolution,
// transfers, archives, docker load and optional re-push.
package app

import (
	"context"
	"dpull/internal/archive"
	"dpull/internal/reference"
	"dpull/internal/registry"
	"dpull/internal/store"
	"dpull/internal/xfer"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Options is the resolved command line configuration.
type Options struct {
	Images       []string
	Tags         []string
	Platform     string
	Mirrors      []string
	Concurrency  int
	PreferIPv4   bool
	Resolves     []string
	DoH          []string
	NoDoH        bool
	ChunkSize    int64
	CacheDir     string
	Output       string
	Format       string // docker-archive | oci | none
	Load         bool
	PushTargets  []string
	Retries      int
	Stall        time.Duration
	RetryWait    time.Duration // base backoff between chunk retries
	StallS       string        // raw --stall value, resolved into Stall
	ChunkSizeS   string        // raw --chunk value, resolved into ChunkSize
	Force        bool
	KeepParts    bool
	KeepArchive  bool
	VerifyCached bool
	CheckDiffIDs bool
	Quiet        bool
	DryRun       bool
	Insecure     bool
	PlainHTTP    []string // hosts explicitly allowed to speak plain http
	SkipTLS      bool
	DockerBin    string
	PruneDays    int
	JSON         bool

	Username string
	Password string
}

// Result describes one completed image.
type Result struct {
	Image     string   `json:"image"`
	Platform  string   `json:"platform"`
	Digest    string   `json:"digest"`
	Tags      []string `json:"tags"`
	Layers    int      `json:"layers"`
	Bytes     int64    `json:"bytes"`
	Archive   string   `json:"archive,omitempty"`
	OCILayout string   `json:"oci_layout,omitempty"`
	Loaded    bool     `json:"loaded"`
	Pushed    []string `json:"pushed,omitempty"`
	Seconds   float64  `json:"seconds"`
}

// Pull runs the whole flow for every requested image.
func Pull(ctx context.Context, o *Options) ([]Result, error) {
	if len(o.Images) == 0 {
		return nil, errors.New("没有指定镜像")
	}
	st, err := store.New(o.CacheDir)
	st.KeepParts = o.KeepParts
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, image := range o.Images {
		res, err := pullOne(ctx, o, st, image)
		if err != nil {
			return results, fmt.Errorf("%s: %w", image, err)
		}
		results = append(results, res)
	}
	if o.JSON {
		b, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(b))
	}
	return results, nil
}

func pullOne(ctx context.Context, o *Options, st *store.Store, image string) (Result, error) {
	started := time.Now()
	ref, err := reference.Parse(image)
	if err != nil {
		return Result{}, err
	}
	eps, err := endpointsFor(o, ref)
	if err != nil {
		return Result{}, err
	}
	cli := newClient(o, eps)

	progress := xfer.NewProgress(os.Stderr, o.Quiet)
	if !o.Quiet {
		fmt.Fprintf(os.Stderr, "解析 %s (%s)\n", ref.String(), orDefault(o.Platform, "本机平台"))
	}
	res, err := cli.Resolve(ctx, ref.Repository, refSpec(ref), o.Platform)
	if err != nil {
		return Result{}, err
	}
	if res.Platform.OS != "" {
		if !o.Quiet {
			fmt.Fprintf(os.Stderr, "平台 %s，%d 层，共 %s\n", res.Platform, len(res.Layers), xfer.HumanBytes(res.TotalBytes))
		}
	}
	items := make([]*xfer.Item, 0, len(res.Layers)+1)
	items = append(items, &xfer.Item{
		Label:  "config " + registry.ShortDigest(res.Config.Digest),
		Digest: res.Config.Digest, Size: res.Config.Size, Kind: "config", Ordinal: -1,
	})
	for i, l := range res.Layers {
		items = append(items, &xfer.Item{
			Label:  fmt.Sprintf("layer %d/%d %s", i+1, len(res.Layers), registry.ShortDigest(l.Digest)),
			Digest: l.Digest, Size: l.Size, Kind: "layer", Ordinal: i,
		})
	}

	if o.DryRun {
		fmt.Printf("%s  platform=%s  digest=%s  layers=%d  size=%s\n",
			ref.String(), res.Platform, firstNonEmpty(res.IndexDigest, res.ManifestDigest),
			len(res.Layers), xfer.HumanBytes(res.TotalBytes))
		for i, l := range res.Layers {
			fmt.Printf("  %d) %s %s\n", i+1, l.Digest, xfer.HumanBytes(l.Size))
		}
		fmt.Printf("  config) %s %s\n", res.Config.Digest, xfer.HumanBytes(res.Config.Size))
		return Result{Image: ref.String(), Platform: res.Platform.String(),
			Digest: firstNonEmpty(res.IndexDigest, res.ManifestDigest), Layers: len(res.Layers),
			Bytes: res.TotalBytes}, nil
	}

	if err := locateAll(ctx, cli, res, items); err != nil {
		return Result{}, err
	}

	d := &xfer.Downloader{
		Store:  st,
		Client: cli,
		Opts: xfer.Options{
			Concurrency:  o.Concurrency,
			ChunkSize:    o.ChunkSize,
			Retries:      o.Retries,
			RetryWait:    o.RetryWait,
			StallTimeout: o.Stall,
			Force:        o.Force,
			VerifyCached: o.VerifyCached,
		},
		Prog: progress,
	}
	progress.Start(items)
	derr := d.Run(ctx, items)
	progress.Stop(derr)
	if derr != nil {
		return Result{}, derr
	}

	cfgBlob, err := st.OpenBlob(res.Config.Digest)
	if err != nil {
		return Result{}, err
	}
	cfgBytes, err := io.ReadAll(cfgBlob)
	cfgBlob.Close()
	if err != nil {
		return Result{}, err
	}
	if p := registry.ConfigOSArch(cfgBytes); p.OS != "" && res.Platform.OS == "" {
		res.Platform = p
	}

	if err := verifyConfigLayers(cfgBytes, res.Layers, o.CheckDiffIDs, st); err != nil {
		return Result{}, err
	}

	img := archive.Image{
		RepoTags:   tagsFor(o, ref),
		Config:     archive.Blob{Digest: res.Config.Digest, Path: st.BlobPath(res.Config.Digest), Size: res.Config.Size},
		Manifest:   res.ManifestBytes,
		ManifestMT: res.MediaType,
	}
	for _, l := range res.Layers {
		img.Layers = append(img.Layers, archive.Blob{Digest: l.Digest, Path: st.BlobPath(l.Digest), Size: l.Size})
	}

	out := Result{
		Image:    ref.String(),
		Platform: res.Platform.String(),
		Digest:   firstNonEmpty(res.IndexDigest, res.ManifestDigest),
		Tags:     img.RepoTags,
		Layers:   len(res.Layers),
		Bytes:    res.TotalBytes,
	}

	format := o.Format
	if format == "" {
		format = "docker-archive"
	}
	archivePath := o.Output
	if format == "docker-archive" && archivePath == "" {
		archivePath = filepath.Join(st.Root, "archives", ref.ShortName()+".tar")
		o.KeepArchive = o.KeepArchive || !o.Load
	}

	if format != "none" {
		writeProg := xfer.NewProgress(os.Stderr, o.Quiet)
		artItem := &xfer.Item{Label: "打包 " + ref.ShortName(), Size: res.TotalBytes}
		writeProg.StartWith([]*xfer.Item{artItem}, "归档")
		var werr error
		switch format {
		case "docker-archive":
			werr = archive.WriteDockerArchive(archivePath, img, func(n int64) { artItem.Done.Add(n) })
		case "oci":
			werr = archive.WriteOCILayout(archivePath, img, func(n int64) { artItem.Done.Add(n) })
		default:
			werr = fmt.Errorf("未知格式 %q", format)
		}
		if werr != nil {
			artItem.SetState(xfer.StateFailed)
		} else {
			artItem.SetState(xfer.StateDone)
		}
		writeProg.Stop(werr)
		if werr != nil {
			return out, werr
		}
		if format == "oci" {
			out.OCILayout = archivePath
		} else {
			out.Archive = archivePath
			if !o.Quiet {
				fmt.Fprintf(os.Stderr, "已写出 docker-archive: %s (%s)\n", archivePath, xfer.HumanBytes(sizeOnDisk(archivePath)))
			}
		}
	}

	if o.Load && format == "docker-archive" {
		if err := dockerLoad(ctx, o.DockerBin, archivePath, !o.Quiet); err != nil {
			// keep the archive: the user can import it once Docker is up
			o.KeepArchive = true
			out.Archive = archivePath
			return out, fmt.Errorf("%w；归档文件已保留在 %s，Docker 启动后可执行: docker load -i %s",
				err, archivePath, archivePath)
		}
		out.Loaded = true
		if !o.KeepArchive && o.Output == "" {
			os.Remove(archivePath)
			out.Archive = ""
		}
	}

	for _, target := range o.PushTargets {
		tref, err := reference.ParseTarget(target)
		if err != nil {
			return out, err
		}
		if err := pushImage(ctx, o, st, res, tref, progress); err != nil {
			return out, fmt.Errorf("推送到 %s 失败: %w", target, err)
		}
		out.Pushed = append(out.Pushed, tref.String())
		if !o.Quiet {
			fmt.Fprintf(os.Stderr, "已推送 %s\n", tref.String())
		}
	}

	// report the digest inside the pushed manifest too
	out.Seconds = time.Since(started).Seconds()
	blobs, parts := st.DiskUsage()
	_ = blobs
	_ = parts
	return out, nil
}

func refSpec(r reference.Ref) string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// tagsFor produces the names `docker load` will apply. Keeping the registry
// host mirrors what `docker pull` shows (docker renders docker.io images in
// their familiar short form), and Docker refuses tag-less references, so a
// digest pull gets a deterministic short tag.
func tagsFor(o *Options, r reference.Ref) []string {
	if len(o.Tags) > 0 {
		return o.Tags
	}
	return []string{r.ForLoad("")}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// locateAll probes blob sizes/range support in parallel.
func locateAll(ctx context.Context, cli *registry.Client, res *registry.Resolved, items []*xfer.Item) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	var pending []*xfer.Item
	for _, it := range items {
		pending = append(pending, it)
	}
	var mu sync.Mutex
	var firstErr error
	var found int64
	for i := range pending {
		wg.Add(1)
		go func(it *xfer.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var desc registry.Descriptor
			if it.Kind == "config" {
				desc = res.Config
			} else {
				desc = res.Layers[it.Ordinal]
			}
			loc, err := cli.LocateBlob(ctx, res.Repo, desc.Digest, desc.URLs)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", it.Label, err)
				}
				mu.Unlock()
				return
			}
			it.Loc = loc
			if loc.Size > 0 && it.Size <= 0 {
				it.Size = loc.Size
			}
			atomic.AddInt64(&found, 1)
		}(pending[i])
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func sizeOnDisk(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

// Prune deletes cached blobs older than days.
func Prune(cacheDir string, days int) (int64, error) {
	st, err := store.New(cacheDir)
	if err != nil {
		return 0, err
	}
	return st.Prune(days)
}
