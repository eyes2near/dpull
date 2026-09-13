package xfer

import (
	"io"
	"testing"
	"time"

	"dpull/internal/registry"
	"dpull/internal/store"
)

func newTestDownloader() *Downloader {
	return &Downloader{
		Opts: Options{Concurrency: 8, ChunkSize: 1 << 20, Retries: 3, StallTimeout: 5 * time.Second},
		Prog: NewProgress(io.Discard, true),
	}
}

func TestPlanForHonoursRangeSupport(t *testing.T) {
	d := newTestDownloader()

	split := d.planFor(&Item{Size: 10 << 20, Loc: registry.BlobLocation{Ranged: true}})
	if split.Count != 10 || split.ChunkSize != 1<<20 {
		t.Errorf("ranged endpoint should be split into 10 chunks, got %+v", split)
	}

	whole := d.planFor(&Item{Size: 10 << 20, Loc: registry.BlobLocation{Ranged: false}})
	if whole.Count != 1 {
		t.Errorf("non-ranged endpoint must stay a single chunk, got %+v", whole)
	}

	small := d.planFor(&Item{Size: 1500, Loc: registry.BlobLocation{Ranged: true}})
	if small.Count != 1 || small.ChunkSize != 1500 {
		t.Errorf("tiny blobs should not be split, got %+v", small)
	}

	unknown := d.planFor(&Item{Size: -1})
	if unknown.Count != 1 || !unknown.Unknown {
		t.Errorf("unknown size should produce one streaming chunk, got %+v", unknown)
	}
}

func TestChunkLenCoversTheWholeBlob(t *testing.T) {
	d := newTestDownloader()
	m := store.Meta{Size: 2500, ChunkSize: 1000, Count: 3}
	var sum int64
	for i := 0; i < m.Count; i++ {
		n := d.chunkLen(m, i)
		if n <= 0 {
			t.Fatalf("chunkLen(%d) = %d", i, n)
		}
		sum += n
	}
	if sum != 2500 {
		t.Errorf("chunks cover %d of 2500 bytes", sum)
	}
	if d.chunkLen(m, 7) != 0 {
		t.Error("chunk beyond the blob should be empty")
	}
}

func TestNormalize(t *testing.T) {
	d := &Downloader{Opts: Options{Concurrency: 0, ChunkSize: 10, Retries: 0}}
	d.normalize()
	if d.Opts.Concurrency < 1 || d.Opts.ChunkSize < 64<<10 || d.Opts.Retries < 1 || d.Opts.StallTimeout <= 0 {
		t.Errorf("normalize left broken defaults: %+v", d.Opts)
	}
}

func TestBackoffGrowsAndIsBounded(t *testing.T) {
	base := 100 * time.Millisecond
	prev := time.Duration(0)
	for i := 1; i <= 8; i++ {
		d := backoff(base, i, io.ErrUnexpectedEOF)
		if d < base {
			t.Errorf("attempt %d: backoff %v below the base wait", i, d)
		}
		if d > 30*time.Second {
			t.Errorf("attempt %d: backoff %v is unbounded", i, d)
		}
		if i > 2 && d < prev/2 {
			t.Errorf("attempt %d: backoff shrank unexpectedly (%v -> %v)", i, prev, d)
		}
		prev = d
	}
	if got := backoff(base, 1, &registry.Error{StatusCode: 429, RetryAfter: 12 * time.Second}); got != 12*time.Second {
		t.Errorf("Retry-After should be honoured, got %v", got)
	}
	if got := backoff(base, 1, &registry.Error{StatusCode: 429, RetryAfter: time.Hour}); got > 2*time.Minute {
		t.Errorf("Retry-After should be capped, got %v", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:     "512B",
		2048:    "2.00KiB",
		5 << 20: "5.00MiB",
		3 << 30: "3.00GiB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestItemPct(t *testing.T) {
	it := &Item{Size: 200}
	if it.Pct() != 0 {
		t.Error("empty item should report 0%")
	}
	it.Done.Store(50)
	if it.Pct() != 0.25 {
		t.Errorf("Pct = %v", it.Pct())
	}
	unknown := &Item{Size: -1}
	if unknown.Pct() != 1 {
		t.Error("unknown-size items are reported as complete")
	}
}
