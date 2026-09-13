package xfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"dpull/internal/registry"
	"dpull/internal/store"
)

// Options configures the transfer engine.
type Options struct {
	Concurrency  int           // total parallel HTTP connections
	ChunkSize    int64         // bytes per ranged request
	Retries      int           // attempts per chunk
	StallTimeout time.Duration // cancel a read that produced no byte for this long
	RetryWait    time.Duration // first backoff step
	VerifyCached bool          // re-hash cached blobs
	Force        bool          // ignore already-downloaded data
}

// Downloader fetches blobs into a store using ranged requests.
type Downloader struct {
	Store  *store.Store
	Client *registry.Client
	Opts   Options
	Prog   *Progress

	mu      sync.Mutex
	noRange map[string]bool

	queue chan *blobPlan
	qWG   sync.WaitGroup
}

// startAssembler runs sha256 assembly in the background so that verified blobs
// overlap with the transfers that are still running.
func (d *Downloader) startAssembler() {
	d.queue = make(chan *blobPlan, 512)
	d.qWG.Add(1)
	go func() {
		defer d.qWG.Done()
		for pl := range d.queue {
			d.assemblePlan(pl)
		}
	}()
}

func (d *Downloader) enqueue(pl *blobPlan) {
	if d.queue == nil {
		return
	}
	select {
	case d.queue <- pl:
	default: // queue full: the final pass will pick it up
	}
}

// assemblePlan assembles a fully downloaded blob exactly once. sync.Once makes
// sure a caller never observes "assembled" before the verification result of
// that assembly is available: the background assembler and the final pass may
// both reach the same blob.
func (d *Downloader) assemblePlan(pl *blobPlan) error {
	if !pl.completed() {
		return nil
	}
	pl.once.Do(func() {
		it := pl.item
		it.SetState(StateAssembling)
		pl.err = d.assemble(it, pl.meta)
		if pl.err != nil {
			it.SetState(StateFailed)
			it.SetErr(pl.err)
			return
		}
		it.SetState(StateDone)
	})
	return pl.err
}

func (d *Downloader) sawNoRange(digest string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.noRange[digest]
}

func (d *Downloader) markNoRange(digest string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.noRange == nil {
		d.noRange = map[string]bool{}
	}
	d.noRange[digest] = true
}

type job struct {
	item  *Item
	plan  *blobPlan
	chunk int
}

type blobPlan struct {
	mu      sync.Mutex
	dir     string
	meta    store.Meta
	done    map[int]bool
	doneLen map[int]int64
	missing []int
	total   int
	item    *Item
	once    sync.Once
	err     error
}

// completed reports whether every chunk of this blob is on disk.
func (pl *blobPlan) completed() bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return len(pl.done) >= pl.total
}

// errNoRange means the server answered a Range request with the whole blob.
// Such an endpoint cannot serve chunked downloads, so the item is retried as a
// single stream.
var errNoRange = errors.New("服务端忽略了 Range 请求")

// Run downloads every item, resuming whatever is already on disk.
func (d *Downloader) Run(ctx context.Context, items []*Item) error {
	if len(items) == 0 {
		return nil
	}
	d.normalize()

	d.startAssembler()
	defer func() {
		if d.queue != nil {
			close(d.queue)
			d.qWG.Wait()
		}
	}()

	plans := make(map[string]*blobPlan, len(items))
	var jobs []job
	for _, it := range items {
		switch {
		case d.Opts.Force:
			if _, err := d.Store.PurgeParts(it.Digest); err != nil {
				return err
			}
			if err := d.Store.DropBlob(it.Digest); err != nil {
				return err
			}
		case d.Store.HasBlob(it.Digest, it.Size, d.Opts.VerifyCached):
			it.SetState(StateCached)
			if it.Size > 0 {
				it.Done.Store(it.Size)
			}
			it.Final = d.Store.BlobPath(it.Digest)
			continue
		}
		meta := d.planFor(it)
		dir, done, err := d.Store.PreparePartsDir(it.Digest, meta)
		if err != nil {
			return err
		}
		pl := &blobPlan{dir: dir, meta: meta, done: done, doneLen: map[int]int64{}, total: meta.Count, item: it}
		plans[it.Digest] = pl
		for i := 0; i < meta.Count; i++ {
			if done[i] {
				pl.doneLen[i] = d.chunkLen(meta, i)
				it.Done.Add(pl.doneLen[i])
				continue
			}
			pl.missing = append(pl.missing, i)
			jobs = append(jobs, job{item: it, plan: pl, chunk: i})
		}
		if len(pl.missing) == 0 {
			// everything was already on disk: verify it in the background
			d.enqueue(pl)
		}
	}

	resumed := 0
	for _, it := range items {
		if it.State() == StateCached {
			resumed++
		} else if it.Done.Load() > 0 {
			resumed++
		}
	}
	if resumed > 0 {
		d.Prog.Note("已命中缓存/断点 %d 个对象，待传输分片 %d 个（每片 %s）",
			resumed, len(jobs), HumanBytes(d.Opts.ChunkSize))
	}

	if len(jobs) > 0 {
		d.runJobs(ctx, jobs)
	}

	// endpoints that turned out not to support ranges: retry the affected items
	// as one sequential stream instead of failing the whole pull
	for _, it := range items {
		if it.State() == StateDone || it.State() == StateCached {
			continue
		}
		if !d.sawNoRange(it.Digest) {
			continue
		}
		d.Prog.Warnf("%s: 该端点不支持 Range 分片，改用单连接整块下载", it.Label)
		if err := d.degrade(ctx, it); err != nil {
			d.Prog.Warnf("%s: 单连接下载也失败了: %v", it.Label, err)
		}
	}

	// assemble in manifest order
	var firstErr error
	for _, it := range items {
		if it.State() == StateDone || it.State() == StateCached {
			continue
		}
		if it.State() == StateFailed {
			if firstErr == nil {
				firstErr = it.Err()
			}
			continue
		}
		pl := plans[it.Digest]
		if pl == nil {
			continue
		}
		if it.State() == StateDone || it.State() == StateCached {
			continue
		}
		if missing := d.missingCount(pl); missing > 0 {
			if it.Err() == nil {
				it.SetErr(fmt.Errorf("%s: 仍有 %d 个分片未完成", it.Label, missing))
			}
			it.SetState(StateFailed)
			if firstErr == nil {
				firstErr = it.Err()
			}
			continue
		}
		if err := d.assemblePlan(pl); err != nil {
			// a corrupt assembly is worth one clean retransmission
			if errors.Is(err, store.ErrMismatch) {
				if rerr := d.assembleRetryable(ctx, it, pl); rerr == nil {
					continue
				}
			}
			it.SetState(StateFailed)
			it.SetErr(err)
			d.Prog.Warnf("%s: %v", it.Label, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}
	return firstErr
}

func (d *Downloader) missingCount(pl *blobPlan) int {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	missing := 0
	for _, i := range pl.missing {
		if !pl.done[i] {
			missing++
		}
	}
	return missing
}

func (d *Downloader) normalize() {
	if d.Opts.Concurrency < 1 {
		d.Opts.Concurrency = 8
	}
	if d.Opts.ChunkSize < 64<<10 {
		d.Opts.ChunkSize = 64 << 10
	}
	if d.Opts.Retries < 1 {
		d.Opts.Retries = 5
	}
	if d.Opts.StallTimeout <= 0 {
		d.Opts.StallTimeout = 30 * time.Second
	}
	if d.Opts.RetryWait <= 0 {
		d.Opts.RetryWait = time.Second
	}
}

func (d *Downloader) planFor(it *Item) store.Meta {
	size := it.Size
	if size < 0 {
		return store.Meta{Digest: it.Digest, Size: -1, ChunkSize: 1 << 30, Count: 1, Unknown: true}
	}
	if size == 0 {
		return store.Meta{Digest: it.Digest, Size: 0, ChunkSize: d.Opts.ChunkSize, Count: 1}
	}
	cs := d.Opts.ChunkSize
	// Small blobs, and endpoints that do not honour Range, stay one chunk:
	// splitting them would write bytes at the wrong offsets.
	if size <= cs*2 || !it.Loc.Ranged {
		cs = size
	}
	count := int((size + cs - 1) / cs)
	return store.Meta{Digest: it.Digest, Size: size, ChunkSize: cs, Count: count}
}

func (d *Downloader) chunkLen(m store.Meta, i int) int64 {
	if m.Size < 0 {
		return 0
	}
	start := int64(i) * m.ChunkSize
	if start >= m.Size {
		return 0
	}
	if m.Size-start < m.ChunkSize {
		return m.Size - start
	}
	return m.ChunkSize
}

func (d *Downloader) runJobs(ctx context.Context, jobs []job) {
	workers := d.Opts.Concurrency
	if workers > len(jobs) {
		workers = len(jobs)
	}
	queue := make(chan job, len(jobs))
	for _, j := range jobs {
		queue <- j
	}
	close(queue)

	var wg sync.WaitGroup
	fail := func(j job, err error) {
		j.item.SetErr(err)
		if j.item.State() != StateDone {
			j.item.SetState(StateFailed)
		}
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				if ctx.Err() != nil {
					return
				}
				if j.item.State() != StateAssembling {
					j.item.SetState(StateActive)
				}
				if _, err := d.fetchChunk(ctx, j); err != nil {
					fail(j, err)
					continue
				}
				d.markDone(j)
			}
		}()
	}
	wg.Wait()

}

func (d *Downloader) markDone(j job) {
	j.plan.mu.Lock()
	j.plan.done[j.chunk] = true
	j.plan.doneLen[j.chunk] = d.chunkLen(j.plan.meta, j.chunk)
	complete := len(j.plan.done) >= j.plan.total
	j.plan.mu.Unlock()
	if complete {
		d.enqueue(j.plan)
	}
}

// fetchChunk downloads one chunk with retries, resuming inside the chunk.
func (d *Downloader) fetchChunk(ctx context.Context, j job) (int64, error) {
	var lastErr error
	for attempt := 1; attempt <= d.Opts.Retries; attempt++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		got, progressed, err := d.attemptChunk(ctx, j)
		if err == nil {
			return got, nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return got, err
		}
		if attempt == d.Opts.Retries {
			break
		}
		wait := backoff(d.Opts.RetryWait, attempt, err)
		d.Prog.Note("%s 分片 %d 第 %d 次重试（%v），等待 %s",
			j.item.Label, j.chunk, attempt, shortErr(err), wait.Round(100*time.Millisecond))
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case <-time.After(wait):
		}
		_ = progressed
	}
	return 0, fmt.Errorf("%s 分片 %d 下载失败: %w", j.item.Label, j.chunk, lastErr)
}

// attemptChunk performs one pass; it always leaves the partial file in place.
func (d *Downloader) attemptChunk(ctx context.Context, j job) (int64, bool, error) {
	it, pl := j.item, j.plan
	m := pl.meta
	path := store.PartPath(pl.dir, j.chunk)
	length := d.chunkLen(m, j.chunk)
	start := int64(j.chunk) * m.ChunkSize

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	have := st.Size()
	if length >= 0 && have > length {
		if err := f.Truncate(length); err != nil {
			return 0, false, err
		}
		have = length
	}
	if length >= 0 && have == length {
		return have, false, store.CommitPart(pl.dir, j.chunk, have, hashFile(path))
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return 0, false, err
	}

	reqStart := start + have
	reqEnd := int64(-1)
	if length >= 0 {
		reqEnd = start + length - 1
	}
	res, err := d.Client.OpenRange(ctx, it.Loc, reqStart, reqEnd)
	if err != nil {
		if errors.Is(err, registry.ErrIgnoredRange) {
			d.markNoRange(it.Digest)
			if have > 0 {
				if terr := f.Truncate(0); terr == nil {
					have = 0
					it.Done.Store(0)
				}
			}
			return have, false, errNoRange
		}
		return have, false, err
	}
	defer res.Close()

	// A server that ignored our Range header would corrupt the chunk.
	if res.Resp.StatusCode == 200 && reqStart > start {
		d.markNoRange(it.Digest)
		if have > 0 {
			if err := f.Truncate(0); err != nil {
				return 0, false, err
			}
			have = 0
			it.Done.Store(0)
		}
		return have, false, errNoRange
	}
	if res.Resp.StatusCode != 206 && res.Resp.StatusCode != 200 {
		return have, false, &registry.Error{StatusCode: res.Resp.StatusCode,
			Endpoint: res.Endpoint.Name, Op: "get blob"}
	}

	w := &progressWriter{f: f, item: it}
	var src io.Reader = res.Resp.Body
	if length >= 0 {
		src = io.LimitReader(src, length-have)
	}
	n, err := copyWithStallWatch(ctx, src, w, d.Opts.StallTimeout)
	it.AddSpeed(n, d.Opts.StallTimeout)
	if err != nil {
		f.Sync()
		return have + n, n > 0, err
	}
	if err := f.Sync(); err != nil {
		return have + n, n > 0, err
	}
	total := have + n
	if length >= 0 {
		if total != length {
			return total, n > 0, fmt.Errorf("连接提前结束: 已收 %d/%d 字节", total, length)
		}
	} else {
		// unknown size: trust the total only if the server closed cleanly
		if res.Resp.ContentLength >= 0 && n != res.Resp.ContentLength && have == 0 {
			return total, n > 0, fmt.Errorf("长度不符: 已收 %d, 声明 %d", n, res.Resp.ContentLength)
		}
	}
	return total, true, store.CommitPart(pl.dir, j.chunk, total, hashFile(path))
}

type progressWriter struct {
	f    *os.File
	item *Item
	n    int64
	last time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.n += int64(n)
	if n > 0 {
		w.item.Done.Add(int64(n))
	}
	return n, err
}

// AddSpeed folds a byte count into the per-item speed estimate.
func (it *Item) AddSpeed(n int64, window time.Duration) {
	if n <= 0 {
		return
	}
	if window <= 0 {
		window = 30 * time.Second
	}
	cur := math.Float64frombits(it.SpeedBits.Load())
	v := float64(n) / window.Seconds()
	it.SpeedBits.Store(math.Float64bits(0.5*cur + 0.5*v))
}

func copyWithStallWatch(ctx context.Context, src io.Reader, dst *progressWriter, stall time.Duration) (int64, error) {
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx2.Done():
				return
			case now := <-t.C:
				if now.Sub(time.Unix(0, last.Load())) > stall {
					cancel()
					return
				}
			}
		}
	}()
	// wrap src to refresh the activity timestamp
	rs := &activityReader{r: src, last: &last}
	buf := make([]byte, 256<<10)
	return io.CopyBuffer(dst, rs, buf)
}

type activityReader struct {
	r    io.Reader
	last *atomic.Int64
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// degrade re-downloads an item as a single un-ranged chunk.
func (d *Downloader) degrade(ctx context.Context, it *Item) error {
	size := it.Size
	if size < 0 {
		size = 0
	}
	m := store.Meta{Digest: it.Digest, Size: it.Size, ChunkSize: size, Count: 1}
	dir, err := d.Store.PurgeParts(it.Digest)
	if err != nil {
		return err
	}
	if _, _, err := d.Store.PreparePartsDir(it.Digest, m); err != nil {
		return err
	}
	it.Done.Store(0)
	it.SetErr(nil)
	pl := &blobPlan{dir: dir, meta: m, done: map[int]bool{}, doneLen: map[int]int64{}, missing: []int{0}, total: 1, item: it}
	d.runJobs(ctx, []job{{item: it, plan: pl, chunk: 0}})
	if !pl.done[0] {
		if it.Err() != nil {
			return it.Err()
		}
		return errors.New("分片未完成")
	}
	return d.assemblePlan(pl)
}

func (d *Downloader) assemble(it *Item, m store.Meta) error {
	p, err := d.Store.Assemble(it.Digest, m.Count, m.Size)
	if err != nil {
		return err
	}
	it.Final = p
	it.SetState(StateDone)
	if m.Size >= 0 {
		it.Done.Store(m.Size)
	}
	return nil
}

// assembleRetryable rebuilds corrupted parts once before giving up.
func (d *Downloader) assembleRetryable(ctx context.Context, it *Item, prev *blobPlan) error {
	m := prev.meta
	d.Prog.Warnf("%s 校验不匹配，重传该对象", it.Label)
	dir, err2 := d.Store.PurgeParts(it.Digest)
	if err2 != nil {
		return err2
	}
	if _, _, err2 := d.Store.PreparePartsDir(it.Digest, m); err2 != nil {
		return err2
	}
	it.Done.Store(0)
	jobs := make([]job, 0, m.Count)
	pl := &blobPlan{dir: dir, meta: m, done: map[int]bool{}, doneLen: map[int]int64{}, total: m.Count, item: it}
	for i := 0; i < m.Count; i++ {
		pl.missing = append(pl.missing, i)
		jobs = append(jobs, job{item: it, plan: pl, chunk: i})
	}
	d.runJobs(ctx, jobs)
	if !pl.completed() {
		if it.Err() != nil {
			return it.Err()
		}
		return fmt.Errorf("%s: 重传后仍缺少分片", it.Label)
	}
	return d.assemblePlan(pl)
}

func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var h hash.Hash = sha256.New()
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// backoff grows exponentially, honours Retry-After and adds jitter.
func backoff(base time.Duration, attempt int, err error) time.Duration {
	var re *registry.Error
	if errors.As(err, &re) && re.RetryAfter > 0 {
		d := re.RetryAfter
		if d > 2*time.Minute {
			d = 2 * time.Minute
		}
		return d
	}
	d := base * time.Duration(1<<uint(min(attempt-1, 5)))
	if d > 20*time.Second {
		d = 20 * time.Second
	}
	jit := time.Duration(rand.Int63n(int64(d)/4 + 1))
	return d + jit
}

func shortErr(err error) string {
	var re *registry.Error
	if errors.As(err, &re) {
		return fmt.Sprintf("%s HTTP %d", re.Op, re.StatusCode)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "网络错误: " + ne.Error()
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
