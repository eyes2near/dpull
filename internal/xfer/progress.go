package xfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dpull/internal/registry"
)

// ItemState describes where a blob currently is.
type ItemState int32

const (
	StatePending ItemState = iota
	StateActive
	StateAssembling
	StateCached
	StateDone
	StateFailed
)

func (s ItemState) String() string {
	switch s {
	case StateActive:
		return "downloading"
	case StateAssembling:
		return "verifying"
	case StateCached:
		return "cached"
	case StateDone:
		return "done"
	}
	return "pending"
}

// Item is one blob being transferred.
type Item struct {
	Label     string // short name for the progress line
	Digest    string
	Size      int64
	Kind      string // "config" or "layer"
	Ordinal   int    // layer index, -1 for the config blob
	Loc       registry.BlobLocation
	Done      atomic.Int64
	SpeedBits atomic.Uint64
	Final     string // path once assembled

	mu    sync.Mutex
	state ItemState
	err   error
}

// State reports the item phase; it is written by workers and read by the UI.
func (it *Item) State() ItemState {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.state
}

// SetState records a phase change.
func (it *Item) SetState(s ItemState) {
	it.mu.Lock()
	it.state = s
	it.mu.Unlock()
}

// Err returns the first error seen for this item.
func (it *Item) Err() error {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.err
}

// SetErr remembers the first error, so a later chunk does not hide it.
func (it *Item) SetErr(err error) {
	if err == nil {
		return
	}
	it.mu.Lock()
	if it.err == nil {
		it.err = err
	}
	it.mu.Unlock()
}

// ResetErr clears the recorded error (used when an item is retried).
func (it *Item) ResetErr() {
	it.mu.Lock()
	it.err = nil
	it.mu.Unlock()
}

// Speed returns the smoothed per-item byte rate.
func (it *Item) Speed() float64 { return math.Float64frombits(it.SpeedBits.Load()) }

// Pct returns the completion ratio, 1 when size is unknown.
func (it *Item) Pct() float64 {
	if it.Size <= 0 {
		return 1
	}
	return float64(it.Done.Load()) / float64(it.Size)
}

// Progress renders a live transfer report.
type Progress struct {
	w        io.Writer
	tty      bool
	quiet    bool
	interval time.Duration

	mu      sync.Mutex
	items   []*Item
	start   time.Time
	prev    int
	stop    chan struct{}
	done    chan struct{}
	running bool
	lastAt  time.Time
	label   string
	lastSum int64
	rate    float64
	termW   int
}

// NewProgress builds a reporter. When quiet, only the final summary is shown.
func NewProgress(w io.Writer, quiet bool) *Progress {
	tty := false
	if f, ok := w.(*os.File); ok {
		tty = isTerminal(f)
	}
	w2, _ := strconv.Atoi(os.Getenv("COLUMNS"))
	if w2 < 40 || w2 > 200 {
		w2 = 100
	}
	return &Progress{w: w, tty: tty, quiet: quiet, interval: 250 * time.Millisecond,
		stop: make(chan struct{}), done: make(chan struct{}), termW: w2}
}

// Start launches the render loop for a download phase.
func (p *Progress) Start(items []*Item) { p.StartWith(items, "层") }

// StartWith launches the render loop and names the phase, so the summary reads
// correctly while packing or pushing. It may be called again after Stop.
func (p *Progress) StartWith(items []*Item, label string) {
	if label == "" {
		label = "对象"
	}
	p.mu.Lock()
	p.items = items
	p.start = time.Now()
	p.lastAt = p.start
	p.prev = 0
	p.label = label
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	stop, done := p.stop, p.done
	running := p.running
	p.running = true
	p.mu.Unlock()
	if running {
		// a previous phase never got Stop(); make sure we do not double close
		close(stop)
		return
	}
	go p.loop(stop, done)
}

// Stop halts rendering and prints the final summary.
func (p *Progress) Stop(err error) {
	p.mu.Lock()
	stop, done, running := p.stop, p.done, p.running
	p.running = false
	p.mu.Unlock()
	if !running {
		return
	}
	close(stop)
	<-done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty && p.prev > 0 {
		fmt.Fprint(p.w, "\n")
		p.prev = 0
	}
	p.summaryLocked(err)
}

func (p *Progress) loop(stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	nonTTYEvery := 10 * time.Second
	lastReport := time.Now()
	for {
		select {
		case <-stopCh:
			return
		case <-t.C:
			if p.tty {
				p.frame()
			} else if !p.quiet && time.Since(lastReport) >= nonTTYEvery {
				lastReport = time.Now()
				p.plain()
			}
		}
	}
}

func (p *Progress) totalsLocked() (total, done int64, active, finished, failed int) {
	for _, it := range p.items {
		if it.Size >= 0 {
			total += it.Size
		}
		done += it.Done.Load()
		switch it.State() {
		case StateDone, StateCached:
			finished++
		case StateFailed:
			failed++
		case StateActive, StateAssembling:
			active++
		}
	}
	return
}

func (p *Progress) updateRate(sum int64) {
	now := time.Now()
	dt := now.Sub(p.lastAt).Seconds()
	if dt >= 0.5 {
		inst := float64(sum-p.lastSum) / dt
		if p.rate == 0 {
			p.rate = inst
		} else {
			p.rate = 0.6*p.rate + 0.4*inst
		}
		if p.rate < 0 {
			p.rate = 0
		}
		p.lastAt = now
		p.lastSum = sum
	}
}

func (p *Progress) frame() {
	p.mu.Lock()
	defer p.mu.Unlock()
	total, done, active, finished, failed := p.totalsLocked()
	p.updateRate(done)

	lines := make([]string, 0, 6)
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total)
	}
	eta := "未知"
	if p.rate > 1024 && total > done {
		eta = (time.Duration(float64(total-done)/p.rate) * time.Second).Round(time.Second).String()
	}
	lines = append(lines, fmt.Sprintf("总进度 %s %s %s/s  剩余 %s  完成 %d/%d%s",
		bar(pct, 24), HumanBytes(done)+"/"+HumanBytes(total), HumanBytes(int64(p.rate)),
		eta, finished, len(p.items),
		func() string {
			if failed > 0 {
				return fmt.Sprintf("  失败 %d", failed)
			}
			return ""
		}()))

	shown := 0
	for _, it := range p.items {
		if shown >= 4 {
			break
		}
		if it.State() != StateActive && it.State() != StateAssembling {
			continue
		}
		st := "下载"
		if it.State() == StateAssembling {
			st = "校验"
		}
		lines = append(lines, fmt.Sprintf("  %s %s %s %s/s  %s",
			st, it.Label, bar(it.Pct(), 18),
			HumanBytes(int64(it.Speed())), HumanBytes(it.Done.Load())+"/"+HumanBytes(it.Size)))
		shown++
	}
	if active == 0 && finished < len(p.items) {
		lines = append(lines, "  准备中…")
	}
	for i := range lines {
		lines[i] = clip(lines[i], p.termW)
	}
	if p.prev > 1 {
		fmt.Fprintf(p.w, "\x1b[%dA", p.prev-1)
	}
	fmt.Fprint(p.w, "\r\x1b[J")
	fmt.Fprint(p.w, strings.Join(lines, "\n"))
	p.prev = len(lines)
}

func (p *Progress) plain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	total, done, _, finished, failed := p.totalsLocked()
	p.updateRate(done)
	fmt.Fprintf(p.w, "[%s] %s/%s (%.0f%%) %s/s  完成 %d/%d %s%s\n",
		time.Since(p.start).Truncate(time.Second), HumanBytes(done), HumanBytes(total),
		100*float64(done)/max1(float64(total)), HumanBytes(int64(p.rate)), finished, len(p.items), orDash(p.label),
		func() string {
			if failed > 0 {
				return fmt.Sprintf("，失败 %d", failed)
			}
			return ""
		}())
}

func (p *Progress) summaryLocked(err error) {
	if p.quiet {
		return
	}
	total, done, _, finished, failed := p.totalsLocked()
	elapsed := time.Since(p.start).Round(time.Second)
	fmt.Fprintf(p.w, "完成 %d/%d %s，共 %s，用时 %s，平均 %s/s",
		finished, len(p.items), orDash(p.label), HumanBytes(done), elapsed,
		HumanBytes(int64(float64(done)/max1(elapsed.Seconds()))))
	if failed > 0 {
		fmt.Fprintf(p.w, "，失败 %d", failed)
	}
	fmt.Fprintln(p.w)
	_ = total
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(p.w, "错误: %v\n", err)
	}
}

// Note prints an informational line without breaking the live frame.
func (p *Progress) Note(format string, args ...any) {
	if p.quiet {
		return
	}
	msg := fmt.Sprintf(format, args...)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty && p.prev > 0 {
		fmt.Fprint(p.w, "\r\x1b[2K")
		fmt.Fprintln(p.w, msg)
		p.prev = 1
		return
	}
	fmt.Fprintln(p.w, msg)
}

// Warnf prints a warning line.
func (p *Progress) Warnf(format string, args ...any) {
	p.Note("警告: "+format, args...)
}

func bar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	return fmt.Sprintf("[%s%s %5.1f%%]", strings.Repeat("=", filled),
		strings.Repeat(" ", width-filled), pct*100)
}

// HumanBytes formats a byte count.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit && n >= 0 {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	f := float64(n)
	for _, u := range units {
		f /= unit
		if f < unit {
			if f < 10 {
				return fmt.Sprintf("%.2f%s", f, u)
			}
			return fmt.Sprintf("%.1f%s", f, u)
		}
	}
	return fmt.Sprintf("%.1fEiB", f/unit)
}

func clip(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return ""
	}
	return string(r[:w-1]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "对象"
	}
	return s
}

func max1(f float64) float64 {
	if f < 1 {
		return 1
	}
	return f
}
