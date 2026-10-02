package ibkr

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseSettledCashScheduleIsStrict(t *testing.T) {
	points, err := parseSettledCashSchedule("20261002:1234.56;20261005:-78.9")
	if err != nil || len(points) != 2 || points[0].Amount != 1234.56 || points[1].Amount != -78.9 ||
		!points[0].Date.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) || !points[1].Date.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("valid schedule: %+v %v", points, err)
	}
	if points, err := parseSettledCashSchedule("20261002:0"); err != nil || len(points) != 1 || points[0].Amount != 0 {
		t.Fatalf("a real zero is an observed balance: %+v %v", points, err)
	}
	long := make([]string, maxSettledCashSchedulePoints+1)
	for i := range long {
		long[i] = time.Date(2026, 10, 1+i, 0, 0, 0, 0, time.UTC).Format("20060102") + ":1"
	}
	for _, value := range []string{
		"", " ", "20261002", "20261002:", ":1", "2026102:1", "202610021:1", "20261302:1", "20260230:1", "2026-10-02:1",
		"20261002:abc", "20261002: 1", "20261002:NaN", "20261002:+Inf", "20261002:" + fmt.Sprint(math.MaxFloat64), "20261002:-" + fmt.Sprint(math.MaxFloat64),
		"20261005:1;20261002:2", "20261002:1;20261002:2", "20261002:1;", ";20261002:1", "20261002:1,20261005:2", strings.Join(long, ";"),
	} {
		if points, err := parseSettledCashSchedule(value); err == nil {
			t.Fatalf("%q parsed as %+v", value, points)
		}
	}
}

func TestSettledCashScheduleCaptureFromAccountStream(t *testing.T) {
	c, binding, account := streamObservationFixture(t)
	feedStreamValue(c.conn, account, "SettledCashByDate", "20261002:100.25;20261005:300.5", "EUR")
	feedStreamValue(c.conn, account, "SettledCashByDate-S", "20261002:90;20261005:300.5", "EUR")
	feedStreamValue(c.conn, account, "$LEDGER-SettledCashByDate", "20261002:500;20261005:200", "USD")
	feedStreamValue(c.conn, account, "SettledCashByDate", "20261002:777;20261005:888", "BASE")
	feedStreamValue(c.conn, account, "SettledCashByDate", "garbled", "CHF")
	feedStreamValue(c.conn, account, "SettledCashByDate-S", "20261002:1", "GBP")
	feedStreamValue(c.conn, "U_FOREIGN", "SettledCashByDate", "20261002:999999", "JPY")
	got := c.CaptureSettledCashSchedulesForSession(binding, account)
	if got == nil || got.StreamStatus != "initial_pending" || got.Truncated {
		t.Fatalf("pending capture: %+v", got)
	}
	eur, usd := got.Schedules["EUR"], got.Schedules["USD"]
	if eur.Status != "observed" || len(eur.Points) != 2 || eur.Points[0].Amount != 100.25 || len(eur.SegmentPoints) != 2 || eur.SegmentPoints[0].Amount != 90 || eur.ReceivedAt.IsZero() {
		t.Fatalf("EUR total and segment: %+v", eur)
	}
	if usd.Status != "observed" || len(usd.Points) != 2 || usd.Points[1].Amount != 200 || usd.SegmentPoints != nil {
		t.Fatalf("10.47 wire prefix lost the USD schedule: %+v", usd)
	}
	if _, ok := got.Schedules["BASE"]; ok {
		t.Fatal("the BASE total became a currency schedule")
	}
	if _, ok := got.Schedules["JPY"]; ok {
		t.Fatal("a foreign account's schedule was captured")
	}
	if s := got.Schedules["CHF"]; s.Status != "invalid" || s.Points != nil || !strings.Contains(s.Reason, "SettledCashByDate") {
		t.Fatalf("garbled schedule: %+v", s)
	}
	if s := got.Schedules["GBP"]; s.Status != "invalid" || s.SegmentPoints != nil {
		t.Fatalf("a segment-only schedule stood in for the account total: %+v", s)
	}
	c.conn.processMessageAtEpoch([]byte("54\x001\x00"+account+"\x00"), binding.epoch)
	if got := c.CaptureSettledCashSchedulesForSession(binding, account); got.StreamStatus != "initial_complete" || len(got.Schedules) != 4 {
		t.Fatalf("completed capture: %+v", got)
	}
	obs := c.CaptureAccountStreamObservationForSession(binding, account)
	sources := map[string]string{}
	for _, row := range obs.Rows {
		sources[row.Key+"/"+row.Currency] = row.Source + "/" + row.ValueStatus
	}
	if sources["SettledCashByDate/EUR"] != "currency_settlement_schedule/observed" || sources["SettledCashByDate/CHF"] != "currency_settlement_schedule/invalid" ||
		sources["SettledCashByDate/BASE"] != "unlabelled/observed" || sources["$LEDGER-SettledCashByDate/USD"] != "currency_settlement_schedule/observed" {
		t.Fatalf("diagnostic rows: %v", sources)
	}

	// A later callback replaces the schedule; the other currencies stay.
	feedStreamValue(c.conn, account, "SettledCashByDate", "20261005:300.5", "EUR")
	if s := c.CaptureSettledCashSchedulesForSession(binding, account).Schedules["EUR"]; len(s.Points) != 1 || s.Points[0].Amount != 300.5 {
		t.Fatalf("update not applied: %+v", s)
	}
	if got := c.CaptureSettledCashSchedulesForSession(binding, "U_OTHER"); got.StreamStatus != "scope_or_generation_changed" || len(got.Schedules) != 0 {
		t.Fatalf("foreign selected scope admitted: %+v", got)
	}
	c.conn.resetPortfolioStreamHealth(account, time.Now().UTC())
	if got := c.CaptureSettledCashSchedulesForSession(binding, account); got.StreamStatus != "initial_pending" || len(got.Schedules) != 0 {
		t.Fatalf("schedules crossed a subscription reset: %+v", got)
	}
	feedStreamValue(c.conn, account, "SettledCashByDate", "20261002:1", "EUR")
	c.conn.resetOrderIDReadiness()
	if c.CaptureSettledCashSchedulesForSession(binding, account) != nil {
		t.Fatal("retired original binding survived")
	}
	newBinding, _ := c.CaptureSession()
	if got := c.CaptureSettledCashSchedulesForSession(newBinding, account); got == nil || got.StreamStatus != "no_subscription" || len(got.Schedules) != 0 {
		t.Fatalf("schedules crossed a socket reset: %+v", got)
	}
}

func TestSettledCashScheduleCaptureIsBounded(t *testing.T) {
	c, binding, account := streamObservationFixture(t)
	for i := range maxSettledCashScheduleCells + 8 {
		feedStreamValue(c.conn, account, "SettledCashByDate", "20261002:1", fmt.Sprintf("%c%cA", 'A'+i/26, 'A'+i%26))
	}
	got := c.CaptureSettledCashSchedulesForSession(binding, account)
	if !got.Truncated || len(got.Schedules) != maxSettledCashScheduleCells {
		t.Fatalf("provider could grow the schedule receipt without bound: truncated=%v n=%d", got.Truncated, len(got.Schedules))
	}
}
