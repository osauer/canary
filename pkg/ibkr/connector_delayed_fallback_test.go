package ibkr

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestDelayedMarketDataFallbackLeaseRearmsOnlyTargetAndRestoresMode(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	seedDelayedFallbackSymbol(t, connector, "SPY")
	connector.rememberMarketDataAbsence("SPY", 354, "untrusted")
	connector.rememberMarketDataAbsence("QQQ", 354, "untrusted")

	release, err := connector.BeginDelayedMarketDataFallback(context.Background(), "SPY")
	if err != nil {
		t.Fatalf("BeginDelayedMarketDataFallback: %v", err)
	}
	if got := connector.marketDataAbsenceFor("SPY"); got != nil {
		release()
		t.Fatalf("target absence was not rearmed: %+v", got)
	}
	if got := connector.marketDataAbsenceFor("QQQ"); got == nil {
		release()
		t.Fatal("fallback cleared an unrelated entitlement observation")
	}
	assertMarketDataModes(t, conn, socket.Bytes(), []int{3})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()
	if sub == nil || sub.ReqID == 0 {
		release()
		t.Fatalf("SPY subscription was not force-refreshed: %+v", sub)
	}
	if sub.Bid != 0 || sub.Ask != 0 || sub.LastPrice != 0 || !sub.LastTickAt.IsZero() || !sub.LastPriceTickAt.IsZero() {
		release()
		t.Fatalf("stale SPY observation survived delayed refresh: %+v", sub)
	}

	release()
	assertMarketDataModes(t, conn, socket.Bytes(), []int{3, 2})
}

func TestDelayedMarketDataFallbackLeasePreservesCompetingSessionMode(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	seedDelayedFallbackSymbol(t, connector, "SPY")
	connector.rememberMarketDataAbsence("SPY", 354, "untrusted")

	release, err := connector.BeginDelayedMarketDataFallback(context.Background(), "SPY")
	if err != nil {
		t.Fatalf("BeginDelayedMarketDataFallback: %v", err)
	}
	conn.markCompetingLiveSession("test")
	release()

	assertMarketDataModes(t, conn, socket.Bytes(), []int{3})
}

func TestDelayedMarketDataFallbackRestoresAbsenceWhenRefreshFails(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	seedDelayedFallbackSymbol(t, connector, "SPY")
	connector.rememberMarketDataAbsence("SPY", 354, "untrusted")
	connector.markSymbolInactive("SPY", "test")

	if release, err := connector.BeginDelayedMarketDataFallback(context.Background(), "SPY"); err == nil {
		if release != nil {
			release()
		}
		t.Fatal("canceled refresh unexpectedly acquired delayed fallback lease")
	}
	if got := connector.marketDataAbsenceFor("SPY"); got == nil || got.Code != 354 {
		t.Fatalf("failed fallback did not restore typed absence: %+v", got)
	}
	assertMarketDataModes(t, conn, socket.Bytes(), []int{3, 2})
}

func seedDelayedFallbackSymbol(t *testing.T, connector *Connector, symbol string) {
	t.Helper()
	if !connector.SeedContractDetails(symbol, ContractDetailsLite{
		Symbol: symbol, SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA",
		Currency: "USD", ConID: 756733, TradingClass: symbol,
	}) {
		t.Fatalf("seed contract details for %s", symbol)
	}
	connector.subMu.Lock()
	connector.subscriptions[symbol] = &Subscription{
		Symbol: symbol, LastTime: time.Now(), LastTickAt: time.Now(), LastPriceTickAt: time.Now(),
		Bid: 100, Ask: 101, LastPrice: 100.5, Observed: true,
		RejectCh: make(chan SubscriptionRejection, 1),
	}
	connector.subMu.Unlock()
}

func assertMarketDataModes(t *testing.T, conn *Connection, payload []byte, want []int) {
	t.Helper()
	var got []int
	for _, frame := range decodeOutboundFrames(t, conn, payload) {
		if len(frame) < 3 || frame[0] != strconv.Itoa(reqMarketDataType) {
			continue
		}
		mode, err := strconv.Atoi(frame[2])
		if err != nil {
			t.Fatalf("decode market-data mode from %#v: %v", frame, err)
		}
		got = append(got, mode)
	}
	if len(got) != len(want) {
		t.Fatalf("market-data mode sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("market-data mode sequence = %v, want %v", got, want)
		}
	}
}
