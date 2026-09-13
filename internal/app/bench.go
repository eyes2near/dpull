package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dpull/internal/reference"
	"dpull/internal/registry"
	"dpull/internal/xfer"
)

// benchRow is one endpoint's measurement result.
type benchRow struct {
	ep     registry.Endpoint
	bytes  int64
	secs   float64
	ranged bool
	err    error
}

func (r benchRow) rate() float64 {
	if r.secs <= 0 {
		return 0
	}
	return float64(r.bytes) / r.secs
}

// Bench measures every candidate endpoint against a real image and ranks them.
// Picking a working mirror is most of the battle on mainland-China networks, so
// this is usually the first command to run.
func Bench(ctx context.Context, o *Options) error {
	if len(o.Images) == 0 {
		return errors.New("bench 需要一个镜像作为测试对象，例如 dpull bench alpine:3.20")
	}
	ref, err := reference.Parse(o.Images[0])
	if err != nil {
		return err
	}
	eps, err := endpointsFor(o, ref)
	if err != nil {
		return err
	}
	probe := o.ChunkSize
	if probe < 1<<20 {
		probe = 4 << 20
	}
	fmt.Printf("测速镜像 %s；每个端点 %d 连接 x %s\n\n", ref.String(), o.Concurrency, xfer.HumanBytes(probe))

	rows := make([]benchRow, len(eps))
	for i, ep := range eps {
		rows[i] = benchOne(ctx, o, ref, ep, probe)
		fmt.Printf("  %-34s", ep.Host)
		if rows[i].err != nil {
			fmt.Printf("  失败: %s\n", shortLine(rows[i].err))
			continue
		}
		fmt.Printf("  %10s/s  %s / %.1fs  支持分片=%v\n",
			xfer.HumanBytes(int64(rows[i].rate())), xfer.HumanBytes(rows[i].bytes),
			rows[i].secs, rows[i].ranged)
	}

	rank := append([]benchRow(nil), rows...)
	sort.SliceStable(rank, func(i, j int) bool { return rank[i].rate() > rank[j].rate() })

	fmt.Println("\n排名:")
	for i, r := range rank {
		status := "可用"
		switch {
		case r.err != nil:
			status = "不可用"
		case r.rate() < 128*1024:
			status = "很慢"
		}
		fmt.Printf("  %d. %-34s %10s/s  %s\n", i+1, r.ep.Host, xfer.HumanBytes(int64(r.rate())), status)
	}
	best := rank[0]
	switch {
	case best.err != nil || best.rate() < 128*1024:
		fmt.Println("\n没有理想端点：换一个 --mirror，或先用 -c 4 降低并发再试。")
	default:
		fmt.Printf("\n建议: dpull pull %s --mirror %s\n", o.Images[0], best.ep.Host)
	}
	return nil
}

// benchOne downloads a few slices of the largest layer from one endpoint.
func benchOne(ctx context.Context, o *Options, ref reference.Ref, ep registry.Endpoint, probe int64) benchRow {
	var row benchRow
	row.ep = ep
	start := time.Now()
	cli := newClient(o, []registry.Endpoint{ep})
	res, err := cli.Resolve(ctx, ref.Repository, refSpec(ref), o.Platform)
	if err != nil {
		row.err = err
		return row
	}
	var pick registry.Descriptor
	for _, l := range res.Layers {
		if l.Size > pick.Size {
			pick = l
		}
	}
	if pick.Digest == "" {
		row.err = errors.New("镜像没有可下载的层")
		return row
	}
	loc, err := cli.LocateBlob(ctx, ref.Repository, pick.Digest, pick.URLs)
	if err != nil {
		row.err = err
		return row
	}
	row.ranged = loc.Ranged
	want := probe
	if pick.Size < want {
		want = pick.Size
	}
	conns := o.Concurrency
	if conns < 2 {
		conns = 2
	}
	var got atomic.Int64
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	for k := 0; k < conns; k++ {
		s := (int64(k) * want) % pick.Size
		e := s + want - 1
		if e >= pick.Size {
			e = pick.Size - 1
		}
		if e < s {
			continue
		}
		wg.Add(1)
		go func(s, e int64) {
			defer wg.Done()
			// a client per connection: pooled idle connections would serialise
			c := newClient(o, []registry.Endpoint{ep})
			cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			resp, err := c.OpenRange(cctx, loc, s, e)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			defer resp.Close()
			buf := make([]byte, 256<<10)
			for {
				n, rerr := resp.Resp.Body.Read(buf)
				got.Add(int64(n))
				if rerr != nil {
					break
				}
			}
		}(s, e)
	}
	wg.Wait()
	row.bytes = got.Load()
	row.secs = time.Since(start).Seconds()
	if row.bytes == 0 && firstErr != nil {
		row.err = firstErr
	}
	return row
}

func shortLine(err error) string {
	s := err.Error()
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}
