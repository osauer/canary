package ibkr

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// One goroutine reads the socket and processes every frame before it reads
// the next (readMessages). When answers arrive late, two causes look alike
// from outside: the terminal sends late, or this reader is behind on the
// frames already sent. On 2026-11-11 and 2026-11-12 answers ran one to four
// minutes late for hours and the log could not tell which. readerLoad keeps
// the reader's own share of the time: how much of it went to processing
// frames, how many there were and of which message. A reader that mostly
// waits on the socket while answers are late points at the terminal; one that
// is mostly processing points at this process.

// readerLoadWindow is the span one window measures. It is a measuring span,
// not a limit: nothing acts on it.
const readerLoadWindow = time.Minute

// readerBusyWarnPct is the share of a window spent processing frames from
// which the reader says so in the log, once per window.
const readerBusyWarnPct = 50

type readerWindow struct {
	span   time.Duration
	busy   time.Duration
	frames int
	byType map[int]int
}

type readerLoad struct {
	mu        sync.Mutex
	start     time.Time
	cur, prev readerWindow
}

// ReaderLoad is the reader's work over the running window and the finished
// one before it.
type ReaderLoad struct {
	Span, Busy time.Duration
	Frames     int
	// Top are the commonest inbound message ids with their frame counts.
	Top []ReaderLoadMessage
}

// ReaderLoadMessage is one inbound message id's frame count.
type ReaderLoadMessage struct {
	ID, Frames int
}

// BusyPct is the share of the span spent processing frames.
func (r ReaderLoad) BusyPct() float64 {
	if r.Span <= 0 {
		return 0
	}
	return min(100, 100*float64(r.Busy)/float64(r.Span))
}

// String is the load in one clause for a log line.
func (r ReaderLoad) String() string {
	if r.Span <= 0 {
		return "the reader has read no frame yet"
	}
	var top []string
	for _, m := range r.Top {
		top = append(top, fmt.Sprintf("message %d: %d", m.ID, m.Frames))
	}
	text := fmt.Sprintf("the reader spent %.0f%% of the last %s processing %d frames", r.BusyPct(), r.Span.Round(time.Second), r.Frames)
	if len(top) > 0 {
		text += " (" + strings.Join(top, ", ") + ")"
	}
	return text
}

// observe counts one processed frame. It reports the finished window when
// this frame turned it.
func (l *readerLoad) observe(msgID int, took time.Duration, now time.Time) (bool, ReaderLoad) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.start.IsZero() {
		l.start = now
	}
	rolled, full := false, ReaderLoad{}
	if span := now.Sub(l.start); span >= readerLoadWindow {
		l.cur.span = span
		rolled, full = true, l.cur.load()
		l.prev, l.cur, l.start = l.cur, readerWindow{}, now
	}
	l.cur.busy += took
	l.cur.frames++
	if l.cur.byType == nil {
		l.cur.byType = map[int]int{}
	}
	l.cur.byType[msgID]++
	return rolled, full
}

// snapshot is the load over the running window and the one before it.
func (l *readerLoad) snapshot(now time.Time) ReaderLoad {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.start.IsZero() {
		return ReaderLoad{}
	}
	both := readerWindow{
		span:   l.prev.span + now.Sub(l.start),
		busy:   l.prev.busy + l.cur.busy,
		frames: l.prev.frames + l.cur.frames,
		byType: map[int]int{},
	}
	for _, w := range []readerWindow{l.prev, l.cur} {
		for id, n := range w.byType {
			both.byType[id] += n
		}
	}
	return both.load()
}

func (w readerWindow) load() ReaderLoad {
	out := ReaderLoad{Span: w.span, Busy: w.busy, Frames: w.frames}
	for id, n := range w.byType {
		out.Top = append(out.Top, ReaderLoadMessage{ID: id, Frames: n})
	}
	slices.SortFunc(out.Top, func(a, b ReaderLoadMessage) int {
		return cmp.Or(cmp.Compare(b.Frames, a.Frames), cmp.Compare(a.ID, b.ID))
	})
	out.Top = out.Top[:min(3, len(out.Top))]
	return out
}
