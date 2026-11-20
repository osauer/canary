package ibkr

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	byType map[int]ReaderLoadMessage
}

type readerLoad struct {
	mu        sync.Mutex
	start     time.Time
	cur, prev readerWindow
	// inFlight is when the frame being processed began (unix nanoseconds,
	// zero between frames), so a reader stuck inside one handler reads as
	// busy while it is stuck, not only once the frame is done.
	inFlight atomic.Int64
}

// ReaderLoad is the reader's work over the running window and the finished
// one before it.
type ReaderLoad struct {
	Span, Busy time.Duration
	Frames     int
	// Running is how long the frame being processed has taken so far; it is
	// counted in Busy.
	Running time.Duration
	// Top are the inbound message ids the reader spent the most time on. Time,
	// not count, ranks them: on 2026-11-19 the commonest frames were ticks,
	// and the count could not say which frames the reader was waiting in.
	Top []ReaderLoadMessage
}

// ReaderLoadMessage is one inbound message id's frame count and the time
// spent processing its frames.
type ReaderLoadMessage struct {
	ID, Frames int
	Busy       time.Duration
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
		top = append(top, fmt.Sprintf("message %d: %d frames, %s", m.ID, m.Frames, m.Busy.Round(time.Millisecond)))
	}
	text := fmt.Sprintf("the reader spent %.0f%% of the last %s processing %d frames", r.BusyPct(), r.Span.Round(time.Second), r.Frames)
	if len(top) > 0 {
		text += " (" + strings.Join(top, ", ") + ")"
	}
	if r.Running >= time.Second {
		text += fmt.Sprintf("; the frame in hand has been processing for %s", r.Running.Round(time.Second))
	}
	return text
}

// begin marks the start of a frame's processing.
func (l *readerLoad) begin(now time.Time) { l.inFlight.Store(now.UnixNano()) }

// observe counts one processed frame. It reports the finished window when
// this frame turned it.
func (l *readerLoad) observe(msgID int, took time.Duration, now time.Time) (bool, ReaderLoad) {
	l.inFlight.Store(0)
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
		l.cur.byType = map[int]ReaderLoadMessage{}
	}
	m := l.cur.byType[msgID]
	m.Frames++
	m.Busy += took
	l.cur.byType[msgID] = m
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
		byType: map[int]ReaderLoadMessage{},
	}
	for _, w := range []readerWindow{l.prev, l.cur} {
		for id, n := range w.byType {
			m := both.byType[id]
			m.Frames += n.Frames
			m.Busy += n.Busy
			both.byType[id] = m
		}
	}
	out := both.load()
	if began := l.inFlight.Load(); began != 0 {
		if running := now.Sub(time.Unix(0, began)); running > 0 {
			out.Running = running
			out.Busy += running
		}
	}
	return out
}

func (w readerWindow) load() ReaderLoad {
	out := ReaderLoad{Span: w.span, Busy: w.busy, Frames: w.frames}
	for id, m := range w.byType {
		m.ID = id
		out.Top = append(out.Top, m)
	}
	slices.SortFunc(out.Top, func(a, b ReaderLoadMessage) int {
		return cmp.Or(cmp.Compare(b.Busy, a.Busy), cmp.Compare(b.Frames, a.Frames), cmp.Compare(a.ID, b.ID))
	})
	out.Top = out.Top[:min(3, len(out.Top))]
	return out
}
