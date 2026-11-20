package ibkr

import (
	"strings"
	"testing"
	"time"
)

// When answers arrive late, the reader's own share of the minute says where
// they queue: a reader that is mostly waiting on the socket points at the
// terminal, one that is mostly processing frames points at this process.
func TestReaderLoadSeparatesAnIdleReaderFromABusyOne(t *testing.T) {
	start := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)

	var idle readerLoad
	for i := range 30 {
		idle.observe(msgTickPrice, time.Millisecond, start.Add(time.Duration(i)*time.Second))
	}
	got := idle.snapshot(start.Add(30 * time.Second))
	if got.Frames != 30 || got.BusyPct() > 1 {
		t.Fatalf("an idle reader read as busy: %+v", got)
	}

	var busy readerLoad
	busy.observe(msgTickPrice, 0, start)
	for i := 1; i <= 100; i++ {
		id := msgTickPrice
		if i%10 == 0 {
			id = msgOpenOrder
		}
		busy.observe(id, 290*time.Millisecond, start.Add(time.Duration(i)*300*time.Millisecond))
	}
	got = busy.snapshot(start.Add(30 * time.Second))
	if got.BusyPct() < 90 {
		t.Fatalf("a reader processing for 29 of 30 seconds read as %.0f%% busy", got.BusyPct())
	}
	if text := got.String(); !strings.Contains(text, "101 frames") || !strings.Contains(text, "message 1: 91 frames, 26.1s") {
		t.Fatalf("the load line does not name the frames and the message that took the time: %s", text)
	}

	// A finished minute is kept beside the running one, so a read just after
	// the turn of a window still sees the load before it.
	rolled, full := busy.observe(msgTickPrice, 0, start.Add(61*time.Second))
	if !rolled || full.Frames != 101 {
		t.Fatalf("the window did not turn after a minute: rolled=%v %+v", rolled, full)
	}
	if got = busy.snapshot(start.Add(62 * time.Second)); got.Frames != 102 {
		t.Fatalf("the turned window dropped the minute before it: %+v", got)
	}
}

// Time ranks the messages, not count: a few frames that each waited seconds
// are where the minute went, however many quick ticks came beside them.
func TestReaderLoadRanksMessagesByTimeSpent(t *testing.T) {
	start := time.Date(2026, 11, 19, 14, 33, 0, 0, time.UTC)
	var l readerLoad
	at := start
	for range 1500 {
		at = at.Add(10 * time.Millisecond)
		l.observe(msgTickPrice, 100*time.Microsecond, at)
	}
	for range 9 {
		at = at.Add(5 * time.Second)
		l.observe(msgOrderStatus, 4*time.Second, at)
	}
	got := l.snapshot(at)
	if len(got.Top) < 2 || got.Top[0].ID != msgOrderStatus || got.Top[0].Busy != 36*time.Second || got.Top[1].ID != msgTickPrice {
		t.Fatalf("messages not ranked by time spent: %+v", got.Top)
	}
	if text := got.String(); !strings.Contains(text, "(message 3: 9 frames, 36s, message 1: 1500 frames, 150ms)") {
		t.Fatalf("the load line does not lead with the message that took the time: %s", text)
	}
}

// A reader stuck inside one handler is the case the figure exists for: it
// must read busy while it is stuck, not only after the frame returns.
func TestReaderLoadCountsTheFrameStillBeingProcessed(t *testing.T) {
	start := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	var l readerLoad
	l.observe(msgTickPrice, time.Millisecond, start)
	l.begin(start.Add(10 * time.Second))
	got := l.snapshot(start.Add(30 * time.Second))
	if got.Running != 20*time.Second || got.BusyPct() < 60 {
		t.Fatalf("a reader 20 s into one frame read as %.0f%% busy, running %s", got.BusyPct(), got.Running)
	}
	if !strings.Contains(got.String(), "the frame in hand has been processing for 20s") {
		t.Fatalf("the load line does not name the stuck frame: %s", got)
	}
	l.observe(msgOpenOrder, 20*time.Second, start.Add(30*time.Second))
	if got = l.snapshot(start.Add(31 * time.Second)); got.Running != 0 {
		t.Fatalf("a finished frame still reads as running: %+v", got)
	}
}
