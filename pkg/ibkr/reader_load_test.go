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
	if text := got.String(); !strings.Contains(text, "101 frames") || !strings.Contains(text, "message 1: 91") {
		t.Fatalf("the load line does not name the frames and the commonest message: %s", text)
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
