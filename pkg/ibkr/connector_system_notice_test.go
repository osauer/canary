package ibkr

import (
	"context"
	"errors"
	"testing"
	"time"
)

func noDefinitionNotice(reqID int) *systemNotification {
	return &systemNotification{
		tickerID: int64(reqID),
		code:     200,
		message:  "No security definition has been found for the request",
	}
}

func TestProcessSystemNoticeSkipsDerivativeInactive(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	c.subMu.Lock()
	c.reqIDMap[5] = "AMD"
	c.subMu.Unlock()
	alias := reqAliasEntry{
		symbol:      "AMD",
		secType:     "OPT",
		localSymbol: "AMD 20251121C250",
	}

	c.processSystemNotice(alias, noDefinitionNotice(5))
	c.processSystemNotice(alias, noDefinitionNotice(5))

	if c.IsSymbolInactive("AMD") {
		t.Fatalf("expected AMD to remain active despite option system notices")
	}
}

// TestProcessSystemNoticeMarksStockInactive pins the record-key/check-key
// contract: the mark lands on the connector's own subscription key, so the
// next SubscribeMarketData is actually suppressed. The former alias-derived
// key (ZVZZT|STK|SMART|DOLLR4LOT|USD|ZVZZT|ZVZZT) matched no check-time key,
// so ZVZZT was re-marked and re-requested every poll cycle. A single notice
// must NOT mark: one code-200 is routinely transient.
func TestProcessSystemNoticeMarksStockInactive(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	c.subMu.Lock()
	c.reqIDMap[7] = "ZVZZT"
	c.subscriptions["ZVZZT"] = &Subscription{Symbol: "ZVZZT", ReqID: 7}
	c.subMu.Unlock()
	alias := reqAliasEntry{symbol: "ZVZZT", secType: "STK", localSymbol: "ZVZZT", tradingClass: "ZVZZT", primaryExch: "DOLLR4LOT"}

	c.processSystemNotice(alias, noDefinitionNotice(7))
	if c.IsSymbolInactive("ZVZZT") {
		t.Fatal("a single definition error must not mark a symbol inactive")
	}

	c.processSystemNotice(alias, noDefinitionNotice(7))
	if !c.IsSymbolInactive("ZVZZT") {
		t.Fatal("expected ZVZZT to be marked inactive after confirmation")
	}

	if err := c.SubscribeMarketData(context.Background(), "ZVZZT", nil); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("expected ErrSymbolInactive from suppressed subscribe, got %v", err)
	}
}

func TestProcessSystemNoticeMarksRoutedStockInactiveByRoute(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	key := MarketDataKeyForContract(Contract{Symbol: "MBG", SecType: "STK", Exchange: "SMART", Currency: "USD"})
	c.subMu.Lock()
	c.reqIDMap[8] = key
	c.subscriptions[key] = &Subscription{Symbol: key, ReqID: 8}
	c.subMu.Unlock()
	alias := reqAliasEntry{
		symbol:   "MBG",
		secType:  "STK",
		exchange: "SMART",
		currency: "USD",
	}

	c.processSystemNotice(alias, noDefinitionNotice(8))
	c.processSystemNotice(alias, noDefinitionNotice(8))

	if c.IsSymbolInactive("MBG") {
		t.Fatalf("bare MBG should remain usable for an explicit non-US route")
	}
	if !c.IsSymbolInactive(key) {
		t.Fatalf("expected failed route %q to be marked inactive", key)
	}
}

// TestProcessSystemNoticeInactiveGuards pins the cases that must not feed
// the inactive map, mirroring the 354 absence guards: reqIDs the connector
// does not own (contract-details probes used to mark alias-derived keys
// here — the observed FX-repair poisoning), and notices during a farm
// impairment.
func TestProcessSystemNoticeInactiveGuards(t *testing.T) {
	t.Run("unowned reqID", func(t *testing.T) {
		c := readyBrokerEvidenceTestConnector(t)
		alias := reqAliasEntry{symbol: "USD", secType: "CASH", exchange: "IDEALPRO", primaryExch: "IDEALPRO", currency: "EUR"}
		c.processSystemNotice(alias, noDefinitionNotice(11))
		c.processSystemNotice(alias, noDefinitionNotice(11))
		c.inactiveMu.RLock()
		marked := len(c.inactiveSymbols)
		c.inactiveMu.RUnlock()
		if marked != 0 {
			t.Fatalf("unowned reqID must not mark anything inactive, got %d entries", marked)
		}
	})

	t.Run("impaired farm", func(t *testing.T) {
		c := readyBrokerEvidenceTestConnector(t)
		c.subMu.Lock()
		c.reqIDMap[12] = "AAPL"
		c.subMu.Unlock()
		c.dataFarmMu.Lock()
		c.dataFarms = map[string]DataFarmStatus{
			dataFarmKey("market", "usfarm"): {Name: "usfarm", Type: "market", Status: "disconnected"},
		}
		c.dataFarmMu.Unlock()
		alias := reqAliasEntry{symbol: "AAPL", secType: "STK"}
		c.processSystemNotice(alias, noDefinitionNotice(12))
		c.processSystemNotice(alias, noDefinitionNotice(12))
		if c.IsSymbolInactive("AAPL") {
			t.Fatal("bounce-window definition errors must not mark a symbol inactive")
		}
	})
}

// TestRegisterInactiveCandidateWindowReset pins the confirmation freshness
// window: occurrences further apart than inactiveCandidateWindow are
// independent transients and must not accumulate into a mark.
func TestRegisterInactiveCandidateWindowReset(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.inactiveMu.Lock()
	c.inactiveCandidates = map[string]inactiveCandidateState{
		"EUR.USD": {count: 1, lastReason: "No security definition has been found", lastUpdated: time.Now().Add(-inactiveCandidateWindow - time.Minute)},
	}
	c.inactiveMu.Unlock()

	if c.registerInactiveCandidate("EUR.USD", "No security definition has been found") {
		t.Fatal("a stale candidate must reset, not confirm")
	}
	if c.IsSymbolInactive("EUR.USD") {
		t.Fatal("EUR.USD must remain active after two occurrences outside the window")
	}
}

func TestProcessSystemNoticeTracksDataFarmProblemAndRecovery(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	brokenAt := time.Date(2026, time.June, 1, 8, 20, 0, 0, time.UTC)
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		code:      2103,
		message:   "Market data farm connection is broken:usopt",
		timestamp: brokenAt,
	})

	farms := c.DataFarmStatuses()
	if len(farms) != 1 {
		t.Fatalf("farms len=%d, want 1: %+v", len(farms), farms)
	}
	if got := farms[0]; got.Name != "usopt" || got.Type != "market" || got.Status != "disconnected" || got.Code != 2103 {
		t.Fatalf("farm = %+v, want usopt market disconnected 2103", got)
	}

	okAt := brokenAt.Add(time.Minute)
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		code:      2104,
		message:   "Market data farm connection is OK:usopt",
		timestamp: okAt,
	})
	farms = c.DataFarmStatuses()
	if len(farms) != 1 {
		t.Fatalf("farms len after OK=%d, want 1: %+v", len(farms), farms)
	}
	if got := farms[0]; got.Name != "usopt" || got.Status != "ok" || got.AsOf != okAt {
		t.Fatalf("farm after OK = %+v, want usopt ok at %s", got, okAt)
	}
}

func TestProcessSystemNoticeTracksConnectivityBreak(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		code:    2110,
		message: "Connectivity between TWS and server is broken",
	})

	farms := c.DataFarmStatuses()
	if len(farms) != 1 {
		t.Fatalf("farms len=%d, want 1: %+v", len(farms), farms)
	}
	if got := farms[0]; got.Name != "tws-server" || got.Type != "connectivity" || got.Status != "broken" {
		t.Fatalf("farm = %+v, want tws-server connectivity broken", got)
	}
}
