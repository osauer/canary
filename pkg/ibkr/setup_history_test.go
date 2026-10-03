package ibkr

import (
	"strconv"
	"testing"
	"time"
)

func TestSetupHistoryRefusesUnboundedOrUnresolvedRequests(t *testing.T) {
	end := time.Now().Add(-time.Hour)
	start := end.Add(-7 * 24 * time.Hour)
	c := Contract{ConID: 17, Symbol: "SYNTH", SecType: "STK", Currency: "USD"}
	if err := validateSetupHistoryWindow(c, start, end); err != nil {
		t.Fatal(err)
	}
	if err := validateSetupHistoryWindow(c, start.Add(-time.Second), end); err == nil {
		t.Fatal("overlong read allowed")
	}
	c.ConID = 0
	if err := validateSetupHistoryWindow(c, start, end); err == nil {
		t.Fatal("unresolved symbol allowed")
	}
	c.ConID = 17
	if err := validateSetupHistoryWindow(c, time.Now(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("future read allowed")
	}
}

func TestSetupHistoryWirePreservesExactIdentityAndCutoff(t *testing.T) {
	c, conn, out, _ := newMissTestConnector(t)
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	end := time.Date(2026, 9, 30, 14, 20, 0, 0, time.UTC)
	start := end.Add(-50 * time.Minute)
	contract := Contract{ConID: 17, Symbol: "SYNTH", SecType: "STK", Currency: "USD", Exchange: "SMART", PrimaryExch: "NASDAQ"}
	type result struct {
		bars []HistoricalBar
		err  error
	}
	done := make(chan result, 1)
	go func() {
		bars, err := c.FetchSetupBars(t.Context(), contract, start, end, 2*time.Second)
		done <- result{bars: bars, err: err}
	}()
	var frame []string
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && frame == nil; {
		for _, candidate := range decodeOutboundFrames(t, conn, out.Bytes()) {
			if candidate[0] == strconv.Itoa(reqHistoricalData) {
				frame = candidate
			}
		}
		if frame == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if len(frame) < 22 {
		t.Fatalf("historical request missing: %v", frame)
	}
	for index, want := range map[int]string{2: "17", 3: "SYNTH", 4: "STK", 9: "SMART", 10: "NASDAQ", 11: "USD", 15: "20260930-14:20:00", 16: "5 mins", 17: "1 D", 18: "1", 19: "TRADES", 20: "2", 21: "0"} {
		if frame[index] != want {
			t.Fatalf("wire field %d = %q, want %q", index, frame[index], want)
		}
	}
	// Extra bars returned by the supplier cannot cross the requested interval.
	fields := []string{strconv.Itoa(msgHistoricalData), frame[1], "3"}
	for _, at := range []time.Time{start.Add(-5 * time.Minute), start, end} {
		fields = append(fields, strconv.FormatInt(at.Unix(), 10), "10", "12", "9", "11", "100", "10.5", "4")
	}
	c.handleHistoricalData(append(fields, ""))
	c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), frame[1], strconv.FormatInt(start.Unix(), 10), strconv.FormatInt(end.Unix(), 10), ""})
	got := <-done
	if got.err != nil || len(got.bars) != 1 || !got.bars[0].Time.Equal(start) {
		t.Fatalf("historical interval changed: %+v", got)
	}
}
