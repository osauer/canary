package ibkr

import (
	"testing"
	"time"
)

// TestMarketDataAbsencesSnapshot pins the observation surface behind
// `canary status`: active records are returned ordered by route key, expired
// ones are pruned on read the same way marketDataAbsenceFor prunes them, and a
// desk with no rejection returns nothing. The snapshot must never disagree
// with what the subscribe paths would suppress.
func TestMarketDataAbsencesSnapshot(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	now := time.Date(2026, 8, 4, 14, 3, 0, 0, time.UTC)
	c.absenceNow = func() time.Time { return now }

	if got := c.MarketDataAbsences(); len(got) != 0 {
		t.Fatalf("quiet connector must report no absence, got %+v", got)
	}

	c.rememberMarketDataAbsence("ZVZZT", 354, "Requested market data is not subscribed.")
	c.rememberMarketDataAbsence("SPX|IND|CBOE", 354, "Requested market data is not subscribed.")

	got := c.MarketDataAbsences()
	if len(got) != 2 {
		t.Fatalf("expected two active absences, got %+v", got)
	}
	if got[0].Key != "SPX|IND|CBOE" || got[1].Key != "ZVZZT" {
		t.Fatalf("absences must be ordered by route key, got %q then %q", got[0].Key, got[1].Key)
	}
	if got[0].Code != 354 || got[0].ObservedAt != now || got[0].RetryAt != now.Add(marketDataAbsenceRetry) {
		t.Fatalf("absence record = %+v, want code 354 observed %s retry %s", got[0], now, now.Add(marketDataAbsenceRetry))
	}

	// One key ages out of its window while the other is re-observed: the
	// snapshot drops the expired record, and the subscribe path agrees.
	now = now.Add(marketDataAbsenceRetry - time.Minute)
	c.rememberMarketDataAbsence("ZVZZT", 354, "Requested market data is not subscribed.")
	now = now.Add(2 * time.Minute)

	got = c.MarketDataAbsences()
	if len(got) != 1 || got[0].Key != "ZVZZT" {
		t.Fatalf("expired record must be pruned, got %+v", got)
	}
	if c.marketDataAbsenceFor("SPX|IND|CBOE") != nil {
		t.Fatal("snapshot and subscribe path disagree about the expired key")
	}
}
