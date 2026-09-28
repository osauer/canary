package ibkr

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The Mini's 2026-09-28 pattern: serialized 25 s timeouts with no answer in
// between. The third, 75 s after the first was sent, declares the stall.
func TestHistoricalStallDeclaredAfterThreeUnansweredOverAMinute(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := &Connector{config: &ConnectorConfig{}}
	t0 := time.Date(2026, 9, 28, 4, 31, 30, 0, time.UTC)

	c.noteHistoricalTimeout(1, 25*time.Second, t0.Add(25*time.Second))
	c.noteHistoricalTimeout(1, 25*time.Second, t0.Add(50*time.Second))
	c.noteHistoricalTimeout(1, 5*time.Second, t0.Add(55*time.Second)) // a tight caller budget proves nothing
	if err := c.admitHistoricalRequest(1, t0.Add(56*time.Second)); err != nil {
		t.Fatalf("two unanswered requests refused history: %v", err)
	}
	c.noteHistoricalTimeout(1, 25*time.Second, t0.Add(75*time.Second))
	if lines := warnLines(buf, "has answered none of the last 3 historical data requests over 1m15s"); len(lines) != 1 {
		t.Fatalf("stall warnings = %q, want one", lines)
	}

	stalledAt := t0.Add(75 * time.Second)
	if err := c.admitHistoricalRequest(1, stalledAt.Add(time.Second)); !errors.Is(err, ErrHistoricalServiceStalled) {
		t.Fatalf("stalled admission = %v, want ErrHistoricalServiceStalled", err)
	}
	c.noteHistoricalTimeout(1, 25*time.Second, stalledAt.Add(20*time.Second)) // a request already in flight
	if lines := warnLines(buf, "has answered none"); len(lines) != 1 {
		t.Fatalf("stall re-announced: %q", lines)
	}

	probe := stalledAt.Add(historicalStallProbeEvery)
	if err := c.admitHistoricalRequest(1, probe); err != nil {
		t.Fatalf("due probe refused: %v", err)
	}
	if err := c.admitHistoricalRequest(1, probe.Add(time.Second)); !errors.Is(err, ErrHistoricalServiceStalled) {
		t.Fatalf("second request in the probe window = %v, want refused", err)
	}

	c.noteHistoricalAnswer(1, probe.Add(3*time.Second))
	if lines := warnLines(buf, "answers historical data requests again after 5m3s"); len(lines) != 1 {
		t.Fatalf("recovery warnings = %q, want one", lines)
	}
	if err := c.admitHistoricalRequest(1, probe.Add(4*time.Second)); err != nil {
		t.Fatalf("recovered admission = %v", err)
	}
}

// Simultaneous timeouts from one hiccup fall inside a minute and must not
// declare a stall; an answer in between restarts the count.
func TestHistoricalStallNeedsASustainedSilence(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := &Connector{config: &ConnectorConfig{}}
	t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)

	for range 5 {
		c.noteHistoricalTimeout(1, 25*time.Second, t0.Add(25*time.Second))
	}
	c.noteHistoricalAnswer(1, t0.Add(30*time.Second))
	c.noteHistoricalTimeout(1, 25*time.Second, t0.Add(80*time.Second))
	c.noteHistoricalTimeout(1, 25*time.Second, t0.Add(105*time.Second))
	if err := c.admitHistoricalRequest(1, t0.Add(106*time.Second)); err != nil {
		t.Fatalf("admission = %v, want no stall", err)
	}
	if lines := warnLines(buf, "has answered none"); len(lines) != 0 {
		t.Fatalf("burst declared a stall: %q", lines)
	}
}

// A reconnected Gateway session starts clean.
func TestHistoricalStallResetsOnNewSession(t *testing.T) {
	c := &Connector{config: &ConnectorConfig{}}
	t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		c.noteHistoricalTimeout(1, 25*time.Second, t0.Add(time.Duration(i)*25*time.Second))
	}
	if err := c.admitHistoricalRequest(1, t0.Add(80*time.Second)); !errors.Is(err, ErrHistoricalServiceStalled) {
		t.Fatalf("admission = %v, want stalled", err)
	}
	if err := c.admitHistoricalRequest(2, t0.Add(81*time.Second)); err != nil {
		t.Fatalf("new session admission = %v, want a fresh start", err)
	}
}

func TestHistoricalAnsweredCountsBrokerAnswersOnly(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, true},
		{&HistoricalRequestError{Code: 162, Message: "Trading TWS session is connected from a different IP address"}, true},
		{&HistoricalRequestError{Code: 200}, true},
		{&HistoricalDataValidationError{Reason: "truncated_bar"}, true},
		{&HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}, false},
		{fmt.Errorf("historical data timeout for SPY after 25s: %w", context.DeadlineExceeded), false},
	} {
		if got := historicalAnswered(tc.err); got != tc.want {
			t.Errorf("historicalAnswered(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

// End to end: a stalled connector refuses history before the wire, lets one
// probe through when due, and an answered probe ends the stall.
func TestStalledConnectorProbesThenRecovers(t *testing.T) {
	buf := captureConnectorLogs(t)
	c, g := newRetiredIdentityConnector(t, "", "756733", 0)
	spy := Contract{ConID: 756733, Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"}
	setStall := func(since, probeAt time.Time) {
		c.historicalStall.mu.Lock()
		c.historicalStall.epoch = g.conn.BrokerSessionEpoch()
		c.historicalStall.since, c.historicalStall.probeAt = since, probeAt
		c.historicalStall.mu.Unlock()
	}
	now := time.Now()
	setStall(now.Add(-2*time.Minute), now.Add(-2*time.Minute))

	_, err := c.fetchHistoricalWithContract(context.Background(), "SPY", spy, 365, 30*time.Second, "TRADES")
	if !errors.Is(err, ErrHistoricalServiceStalled) {
		t.Fatalf("stalled read = %v, want ErrHistoricalServiceStalled", err)
	}
	if since, stalled := c.HistoricalServiceStalled(); !stalled || since.IsZero() {
		t.Fatal("HistoricalServiceStalled() = false while stalled")
	}
	g.mu.Lock()
	sent := len(g.history)
	g.mu.Unlock()
	if sent != 0 {
		t.Fatalf("stalled read sent %d history requests, want none", sent)
	}

	setStall(now.Add(-7*time.Minute), now.Add(-6*time.Minute))
	bars, err := c.fetchHistoricalWithContract(context.Background(), "SPY", spy, 365, 30*time.Second, "TRADES")
	if err != nil || len(bars) != 2 {
		t.Fatalf("probe read = %d bars, %v; want the answer", len(bars), err)
	}
	if _, stalled := c.HistoricalServiceStalled(); stalled {
		t.Fatal("an answered probe left the stall in place")
	}
	if lines := warnLines(buf, "answers historical data requests again"); len(lines) != 1 {
		t.Fatalf("recovery warnings = %q, want one", lines)
	}
}
